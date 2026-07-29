package v1

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// PlatformCapabilities records which platforms are servable, aggregated across every configured
// buildkit pool. It is populated once at controller startup from config.Buildkit and consulted by
// the ImageBuild/ImageCache validating webhooks so that a request for an unavailable platform is
// rejected at admission time rather than left to hang on a pod that can never be scheduled.
//
// This is a webhook-internal helper type, not a CRD schema type.
// +kubebuilder:object:generate=false
type PlatformCapabilities struct {
	// platforms maps a normalized platform string to the pool names that can serve it.
	platforms map[string][]string
	// poolSize maps a pool name to the number of platforms it declares, used to prefer a pool that
	// serves fewer platforms (more likely native) over one serving several (more likely emulated)
	// when a platform is covered by more than one pool.
	poolSize map[string]int
}

// NewPlatformCapabilities builds a PlatformCapabilities from a set of pool name -> platforms
// mappings, as declared in config.Buildkit.Pools().
func NewPlatformCapabilities(poolPlatforms map[string][]string) PlatformCapabilities {
	platforms := make(map[string][]string)
	poolSize := make(map[string]int, len(poolPlatforms))

	for pool, ps := range poolPlatforms {
		for _, p := range ps {
			norm, err := NormalizePlatform(p)
			if err != nil {
				continue
			}
			platforms[norm] = append(platforms[norm], pool)
			poolSize[pool]++
		}
	}

	return PlatformCapabilities{platforms: platforms, poolSize: poolSize}
}

// Supports reports whether the given (normalized or not) platform is served by any pool.
func (c PlatformCapabilities) Supports(platform string) bool {
	norm, err := NormalizePlatform(platform)
	if err != nil {
		return false
	}
	_, ok := c.platforms[norm]
	return ok
}

// Available returns every platform served by at least one pool, sorted for stable error messages.
func (c PlatformCapabilities) Available() []string {
	available := make([]string, 0, len(c.platforms))
	for p := range c.platforms {
		available = append(available, p)
	}
	sort.Strings(available)

	return available
}

// PoolsFor returns the pool names capable of serving the given platform.
func (c PlatformCapabilities) PoolsFor(platform string) []string {
	norm, err := NormalizePlatform(platform)
	if err != nil {
		return nil
	}
	return c.platforms[norm]
}

// ResolvePools assigns each of the given platforms to a single serving pool, preferring whichever
// pool declares the fewest total platforms (more likely native) over one declaring several (more
// likely emulated) when a platform is covered by more than one pool. It returns a pool name ->
// assigned platforms grouping. Callers dispatch a single solve per group; a result with exactly one
// group means every requested platform can be served by one pool (a single multi-platform solve
// suffices), while more than one group means the request must fan out across pools.
//
// Every platform is guaranteed to resolve to some pool by the time this is called in the dispatcher,
// since the validating webhook already rejected any platform unsupported by every configured pool
// (see validatePlatforms) - the error return here is a defensive backstop, not an expected path.
func (c PlatformCapabilities) ResolvePools(platforms []string) (map[string][]string, error) {
	groups := make(map[string][]string)

	for _, platform := range platforms {
		norm, err := NormalizePlatform(platform)
		if err != nil {
			return nil, fmt.Errorf("platform %q is malformed: %w", platform, err)
		}

		pools := c.platforms[norm]
		if len(pools) == 0 {
			return nil, fmt.Errorf("platform %q is not served by any configured pool", platform)
		}

		best := pools[0]
		for _, pool := range pools[1:] {
			if c.poolSize[pool] < c.poolSize[best] {
				best = pool
			}
		}

		groups[best] = append(groups[best], norm)
	}

	return groups, nil
}

// platformCapabilities is populated once at manager startup via SetPlatformCapabilities, before
// webhooks begin serving. It defaults to an empty value, under which no platform is considered
// available, matching the fail-closed behavior required when no pools have been configured.
var platformCapabilities PlatformCapabilities

// SetPlatformCapabilities installs the capabilities consulted by the ImageBuild/ImageCache
// validating webhooks. Must be called once during manager setup, before the webhook server starts.
func SetPlatformCapabilities(c PlatformCapabilities) {
	platformCapabilities = c
}

// NormalizePlatform lowercases and validates a platform string of the form "os/arch[/variant]".
func NormalizePlatform(platform string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(platform))
	parts := strings.Split(trimmed, "/")

	if len(parts) < 2 || len(parts) > 3 {
		return "", fmt.Errorf("must use \"os/arch\" or \"os/arch/variant\" syntax, e.g. \"linux/amd64\"")
	}
	if slices.Contains(parts, "") {
		return "", fmt.Errorf("must not contain empty segments")
	}

	return trimmed, nil
}

// normalizePlatforms lowercases/trims each entry and drops duplicates (by normalized form),
// preserving the first occurrence's original casing/spelling so malformed entries still surface
// exactly as the user wrote them in validation errors. A nil/empty input is returned unchanged,
// so builds that don't request any platform keep taking the pre-multi-arch code path untouched.
func normalizePlatforms(platforms []string) []string {
	if len(platforms) == 0 {
		return platforms
	}

	seen := make(map[string]bool, len(platforms))
	out := make([]string, 0, len(platforms))

	for _, platform := range platforms {
		key := strings.ToLower(strings.TrimSpace(platform))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, platform)
	}

	return out
}

// validatePlatforms checks that every entry in platforms is well-formed and, when caps has any
// declared capabilities, that it's covered by at least one configured pool.
func validatePlatforms(log logr.Logger, fp *field.Path, platforms []string, caps PlatformCapabilities) field.ErrorList {
	var errs field.ErrorList

	seen := make(map[string]bool, len(platforms))

	for idx, platform := range platforms {
		ifp := fp.Index(idx)

		norm, err := NormalizePlatform(platform)
		if err != nil {
			log.V(1).Info("Platform is malformed", "platform", platform)
			errs = append(errs, field.Invalid(ifp, platform, err.Error()))
			continue
		}

		if seen[norm] {
			log.V(1).Info("Platform is duplicated", "platform", platform)
			errs = append(errs, field.Duplicate(ifp, platform))
			continue
		}
		seen[norm] = true

		if !caps.Supports(norm) {
			log.V(1).Info("Platform is not served by any configured pool", "platform", platform)
			errs = append(errs, field.Invalid(ifp, platform, fmt.Sprintf(
				"not served by any configured buildkit pool; available platforms: %v", caps.Available(),
			)))
		}
	}

	return errs
}
