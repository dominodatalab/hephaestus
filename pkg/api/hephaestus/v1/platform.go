package v1

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/containerd/platforms"
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

// ResolvePools assigns the given platforms to the serving pool(s) needed to build all of them. If
// any single configured pool serves every one of them, that one pool is preferred over splitting
// the request, even if a per-platform match would otherwise favor a different (smaller) pool for
// some of them - this is what keeps a request that doesn't need to fan out from being fragmented
// across pools it doesn't have to touch. Otherwise, each platform is assigned independently to
// whichever of its serving pools declares the fewest total platforms (more likely native, over one
// declaring several, more likely emulated). It returns a pool name -> assigned platforms grouping.
// Callers dispatch a single solve per group; a result with exactly one group means a single
// multi-platform solve suffices, while more than one group means the request must fan out across
// pools. Ties (equal candidate pool sizes) break on pool name, so the choice is stable across
// process restarts regardless of map iteration order.
//
// Every platform is guaranteed to resolve to some pool by the time this is called in the dispatcher,
// since the validating webhook already rejected any platform unsupported by every configured pool
// (see validatePlatforms) - the error return here is a defensive backstop, not an expected path.
func (c PlatformCapabilities) ResolvePools(platforms []string) (map[string][]string, error) {
	norm := make([]string, len(platforms))
	for i, platform := range platforms {
		n, err := NormalizePlatform(platform)
		if err != nil {
			return nil, fmt.Errorf("platform %q is malformed: %w", platform, err)
		}
		if len(c.platforms[n]) == 0 {
			return nil, fmt.Errorf("platform %q is not served by any configured pool", platform)
		}
		norm[i] = n
	}

	if pool, ok := c.singlePoolServing(norm); ok {
		return map[string][]string{pool: norm}, nil
	}

	groups := make(map[string][]string)
	for _, platform := range norm {
		pool := c.bestPool(c.platforms[platform])
		groups[pool] = append(groups[pool], platform)
	}

	return groups, nil
}

// singlePoolServing returns the preferred pool that alone serves every one of the given
// (already normalized, possibly duplicated) platforms, if one exists.
func (c PlatformCapabilities) singlePoolServing(platforms []string) (string, bool) {
	unique := make(map[string]bool, len(platforms))
	for _, platform := range platforms {
		unique[platform] = true
	}
	if len(unique) == 0 {
		return "", false
	}

	served := make(map[string]int)
	for platform := range unique {
		for _, pool := range c.platforms[platform] {
			served[pool]++
		}
	}

	var candidates []string
	for pool, count := range served {
		if count == len(unique) {
			candidates = append(candidates, pool)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}

	return c.bestPool(candidates), true
}

// bestPool returns the preferred pool among candidates: the one declaring the fewest total
// platforms (more likely native, over one declaring several, more likely emulated), breaking ties
// by name so the pick is deterministic across process restarts rather than depending on map
// iteration order.
func (c PlatformCapabilities) bestPool(candidates []string) string {
	best := candidates[0]
	for _, pool := range candidates[1:] {
		if c.poolSize[pool] < c.poolSize[best] || (c.poolSize[pool] == c.poolSize[best] && pool < best) {
			best = pool
		}
	}
	return best
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

// NormalizePlatform validates a platform string of the form "os/arch[/variant]" and returns its
// canonical form, applying the same OS/arch aliasing (e.g. "x86_64" -> "amd64", "aarch64" ->
// "arm64") that BuildKit's own solver applies via containerd/platforms to these same strings, so a
// platform string accepted here parses identically once it reaches BuildKit.
func NormalizePlatform(platform string) (string, error) {
	trimmed := strings.TrimSpace(platform)
	parts := strings.Split(trimmed, "/")

	if len(parts) < 2 || len(parts) > 3 {
		return "", fmt.Errorf("must use \"os/arch\" or \"os/arch/variant\" syntax, e.g. \"linux/amd64\"")
	}
	if slices.Contains(parts, "") {
		return "", fmt.Errorf("must not contain empty segments")
	}

	p, err := platforms.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("must use \"os/arch\" or \"os/arch/variant\" syntax, e.g. \"linux/amd64\": %w", err)
	}

	return platforms.Format(p), nil
}

// normalizePlatforms drops duplicates (by normalized form, keeping the first occurrence) and
// rewrites every well-formed entry to its canonical "os/arch[/variant]" form via NormalizePlatform,
// so a value with stray whitespace, mixed case, or an alias like "x86_64" doesn't survive into the
// persisted spec and reach BuildKit's solver un-normalized. A malformed entry is kept as-written so
// it still surfaces exactly as the user typed it in the validation error that follows defaulting. A
// nil/empty input is returned unchanged, so builds that don't request any platform keep taking the
// pre-multi-arch code path untouched.
func normalizePlatforms(platforms []string) []string {
	if len(platforms) == 0 {
		return platforms
	}

	seen := make(map[string]bool, len(platforms))
	out := make([]string, 0, len(platforms))

	for _, platform := range platforms {
		// The dedup key must be the same canonical form written to out, or an entry that's only a
		// duplicate once aliased (e.g. "linux/x86_64" vs "linux/amd64") survives dedup here as two
		// distinct-looking entries that both resolve to the same platform, and fails validation's own
		// (alias-aware) duplicate check right after - instead of being silently deduped like same-form
		// duplicates already are.
		norm, err := NormalizePlatform(platform)
		key := norm
		if err != nil {
			key = strings.ToLower(strings.TrimSpace(platform))
		}

		if seen[key] {
			continue
		}
		seen[key] = true

		if err == nil {
			out = append(out, norm)
		} else {
			out = append(out, platform)
		}
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
