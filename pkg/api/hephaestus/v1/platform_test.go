package v1

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestNormalizePlatform(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		for _, tt := range []struct {
			in, out string
		}{
			{"linux/amd64", "linux/amd64"},
			{"Linux/AMD64", "linux/amd64"},
			{" linux/arm64/v8 ", "linux/arm64/v8"},
			{"linux/x86_64", "linux/amd64"},
			{"linux/aarch64", "linux/arm64"},
		} {
			actual, err := NormalizePlatform(tt.in)
			assert.NoError(t, err)
			assert.Equal(t, tt.out, actual)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		for _, in := range []string{"", "linux", "linux/", "/amd64", "a/b/c/d", "linux//amd64"} {
			_, err := NormalizePlatform(in)
			assert.Error(t, err, "expected error for %q", in)
		}
	})
}

func TestPlatformCapabilities(t *testing.T) {
	caps := NewPlatformCapabilities(map[string][]string{
		"amd64": {"linux/amd64"},
		"multi": {"linux/amd64", "linux/arm64/v8", "bad-entry"},
	})

	assert.True(t, caps.Supports("linux/amd64"))
	assert.True(t, caps.Supports("LINUX/AMD64"))
	assert.True(t, caps.Supports("linux/arm64/v8"))
	assert.False(t, caps.Supports("linux/arm64"))
	assert.False(t, caps.Supports("windows/amd64"))

	assert.ElementsMatch(t, []string{"amd64", "multi"}, caps.PoolsFor("linux/amd64"))
	assert.ElementsMatch(t, []string{"multi"}, caps.PoolsFor("linux/arm64/v8"))
	assert.Nil(t, caps.PoolsFor("windows/amd64"))

	assert.Equal(t, []string{"linux/amd64", "linux/arm64/v8"}, caps.Available())
}

func TestPlatformCapabilitiesResolvePools(t *testing.T) {
	t.Run("single pool covers everything", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{
			"emulated": {"linux/amd64", "linux/arm64"},
		})

		groups, err := caps.ResolvePools([]string{"linux/amd64", "linux/arm64"})
		assert.NoError(t, err)
		assert.Len(t, groups, 1)
		assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, groups["emulated"])
	})

	t.Run("prefers the more specific (native) pool when platforms overlap", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{
			"amd64-native": {"linux/amd64"},
			"emulated":     {"linux/amd64", "linux/arm64"},
		})

		groups, err := caps.ResolvePools([]string{"linux/amd64"})
		assert.NoError(t, err)
		assert.Equal(t, map[string][]string{"amd64-native": {"linux/amd64"}}, groups)
	})

	t.Run("spans multiple pools when no single pool covers everything", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{
			"amd64-native": {"linux/amd64"},
			"arm64-native": {"linux/arm64"},
		})

		groups, err := caps.ResolvePools([]string{"linux/amd64", "linux/arm64"})
		assert.NoError(t, err)
		assert.Len(t, groups, 2)
		assert.Equal(t, []string{"linux/amd64"}, groups["amd64-native"])
		assert.Equal(t, []string{"linux/arm64"}, groups["arm64-native"])
	})

	t.Run("errors on an unresolvable platform", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{"amd64": {"linux/amd64"}})

		_, err := caps.ResolvePools([]string{"linux/arm64"})
		assert.Error(t, err)
	})

	t.Run("errors on a malformed platform", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{"amd64": {"linux/amd64"}})

		_, err := caps.ResolvePools([]string{"not-a-platform"})
		assert.Error(t, err)
	})

	t.Run("does not fragment a request a single pool could serve", func(t *testing.T) {
		// amd64-native alone serves linux/amd64, and would win a per-platform match for it (smaller
		// poolSize than emulated), but emulated alone can serve the whole request; it should be
		// preferred over splitting the request across both pools.
		caps := NewPlatformCapabilities(map[string][]string{
			"amd64-native": {"linux/amd64"},
			"emulated":     {"linux/amd64", "linux/arm64"},
		})

		groups, err := caps.ResolvePools([]string{"linux/amd64", "linux/arm64"})
		assert.NoError(t, err)
		assert.Equal(t, map[string][]string{"emulated": {"linux/amd64", "linux/arm64"}}, groups)
	})

	t.Run("tie-break between equally-sized pools is deterministic", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{
			"z-pool": {"linux/amd64"},
			"a-pool": {"linux/amd64"},
		})

		for range 20 {
			groups, err := caps.ResolvePools([]string{"linux/amd64"})
			assert.NoError(t, err)
			assert.Equal(t, map[string][]string{"a-pool": {"linux/amd64"}}, groups)
		}
	})

	t.Run("empty input resolves to no groups", func(t *testing.T) {
		caps := NewPlatformCapabilities(map[string][]string{"amd64": {"linux/amd64"}})

		groups, err := caps.ResolvePools(nil)
		assert.NoError(t, err)
		assert.Empty(t, groups)
	})
}

func TestPlatformCapabilitiesEmpty(t *testing.T) {
	var caps PlatformCapabilities

	assert.False(t, caps.Supports("linux/amd64"))
	assert.Empty(t, caps.Available())
}

func TestValidatePlatforms(t *testing.T) {
	caps := NewPlatformCapabilities(map[string][]string{"amd64": {"linux/amd64"}})
	fp := field.NewPath("spec").Child("platforms")

	t.Run("empty is valid", func(t *testing.T) {
		assert.Empty(t, validatePlatforms(logr.Discard(), fp, nil, caps))
	})

	t.Run("supported platform is valid", func(t *testing.T) {
		assert.Empty(t, validatePlatforms(logr.Discard(), fp, []string{"linux/amd64"}, caps))
	})

	t.Run("malformed platform is invalid", func(t *testing.T) {
		errs := validatePlatforms(logr.Discard(), fp, []string{"not-a-platform"}, caps)
		assert.Len(t, errs, 1)
	})

	t.Run("unsupported platform is invalid", func(t *testing.T) {
		errs := validatePlatforms(logr.Discard(), fp, []string{"linux/arm64"}, caps)
		assert.Len(t, errs, 1)
	})

	t.Run("duplicate platform is invalid", func(t *testing.T) {
		errs := validatePlatforms(logr.Discard(), fp, []string{"linux/amd64", "LINUX/AMD64"}, caps)
		assert.Len(t, errs, 1)
	})
}

func TestNormalizePlatforms(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		assert.Nil(t, normalizePlatforms(nil))
	})

	t.Run("empty stays empty", func(t *testing.T) {
		result := normalizePlatforms([]string{})
		assert.Empty(t, result)
	})

	t.Run("dedupes by normalized form, keeps first spelling", func(t *testing.T) {
		result := normalizePlatforms([]string{"linux/amd64", "LINUX/AMD64", "linux/arm64"})
		assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, result)
	})

	t.Run("rewrites well-formed entries to their canonical form", func(t *testing.T) {
		result := normalizePlatforms([]string{" LINUX/ARM64 ", "linux/x86_64"})
		assert.Equal(t, []string{"linux/arm64", "linux/amd64"}, result)
	})

	t.Run("keeps a malformed entry as-written", func(t *testing.T) {
		result := normalizePlatforms([]string{"not-a-platform"})
		assert.Equal(t, []string{"not-a-platform"}, result)
	})
}
