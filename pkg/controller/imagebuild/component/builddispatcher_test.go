package component

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dominodatalab/hephaestus/pkg/buildkit/worker"
	"github.com/dominodatalab/hephaestus/pkg/config"
)

// newTestDispatcher builds a BuildDispatcherComponent with caps derived from cfg, mirroring what
// the BuildDispatcher constructor does, since resolvePlatformGroups reads the cached caps rather
// than recomputing them from cfg on every call.
func newTestDispatcher(cfg config.Buildkit) *BuildDispatcherComponent {
	return &BuildDispatcherComponent{cfg: cfg, caps: cfg.PlatformCapabilities()}
}

func TestResolvePlatformGroups(t *testing.T) {
	t.Run("empty platforms resolves to the primary pool", func(t *testing.T) {
		c := newTestDispatcher(config.Buildkit{})
		c.primaryPoolName = "default"

		groups, err := c.resolvePlatformGroups(nil)
		assert.NoError(t, err)
		assert.Equal(t, map[string][]string{"default": nil}, groups)
	})

	t.Run("single pool covering every requested platform", func(t *testing.T) {
		c := newTestDispatcher(config.Buildkit{
			PlatformPools: []config.PlatformPool{
				{Name: "emulated", Platforms: []string{"linux/amd64", "linux/arm64"}},
			},
		})

		groups, err := c.resolvePlatformGroups([]string{"linux/amd64", "linux/arm64"})
		assert.NoError(t, err)
		assert.Len(t, groups, 1)
	})

	t.Run("platforms spanning multiple pools resolve to multiple groups", func(t *testing.T) {
		c := newTestDispatcher(config.Buildkit{
			PlatformPools: []config.PlatformPool{
				{Name: "amd64-native", Platforms: []string{"linux/amd64"}},
				{Name: "arm64-native", Platforms: []string{"linux/arm64"}},
			},
		})

		groups, err := c.resolvePlatformGroups([]string{"linux/amd64", "linux/arm64"})
		assert.NoError(t, err)
		assert.Len(t, groups, 2)
	})

	t.Run("unresolvable platform fails", func(t *testing.T) {
		c := newTestDispatcher(config.Buildkit{
			PlatformPools: []config.PlatformPool{
				{Name: "amd64-native", Platforms: []string{"linux/amd64"}},
			},
		})

		_, err := c.resolvePlatformGroups([]string{"linux/riscv64"})
		assert.Error(t, err)
	})
}

func TestBuildDispatcherCachesPlatformCapabilities(t *testing.T) {
	cfg := config.Buildkit{
		PlatformPools: []config.PlatformPool{
			{Name: "amd64-native", Platforms: []string{"linux/amd64"}},
		},
	}

	c := BuildDispatcher(cfg, worker.PoolSet{}, nil, nil)
	_, err := c.resolvePlatformGroups([]string{"linux/amd64"})
	assert.NoError(t, err)
}

func TestFlattenPlatformAssignments(t *testing.T) {
	groups := map[string][]string{
		"arm64-native": {"linux/arm64"},
		"amd64-native": {"linux/amd64"},
	}

	assignments := flattenPlatformAssignments(groups)

	assert.Equal(t, []platformAssignment{
		{pool: "amd64-native", platform: "linux/amd64"},
		{pool: "arm64-native", platform: "linux/arm64"},
	}, assignments)
}

func TestPlatformImageSlug(t *testing.T) {
	assert.Equal(t, "linux-amd64", platformImageSlug("linux/amd64"))
	assert.Equal(t, "linux-arm64-v8", platformImageSlug("linux/arm64/v8"))
}

func TestSuffixImageRef(t *testing.T) {
	t.Run("tagged reference gets the slug appended to its tag", func(t *testing.T) {
		ref := suffixImageRef("registry.example.com/repo:v1", "linux-arm64")
		assert.Equal(t, "registry.example.com/repo:v1-linux-arm64", ref)
	})

	t.Run("untagged reference gets the slug appended to the implicit tag", func(t *testing.T) {
		ref := suffixImageRef("registry.example.com/repo", "linux-arm64")
		assert.Equal(t, "registry.example.com/repo:latest-linux-arm64", ref)
	})

	t.Run("digest reference falls back to tagging the repository with the default tag and slug", func(t *testing.T) {
		ref := suffixImageRef("registry.example.com/repo@sha256:"+
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "linux-arm64")
		assert.Equal(t, "registry.example.com/repo:latest-linux-arm64", ref)

		parsed, err := name.ParseReference(ref)
		require.NoError(t, err)
		assert.Equal(t, "registry.example.com/repo", parsed.Context().Name())
	})
}

func TestSuffixImageRefs(t *testing.T) {
	images := []string{"registry.example.com/repo:v1", "registry.example.com/other:latest"}

	suffixed := suffixImageRefs(images, "linux/arm64/v8")

	assert.Equal(t, []string{
		"registry.example.com/repo:v1-linux-arm64-v8",
		"registry.example.com/other:latest-linux-arm64-v8",
	}, suffixed)
}

func TestFirstBuildError(t *testing.T) {
	t.Run("nil when every build succeeded", func(t *testing.T) {
		builds := []platformBuild{{platform: "linux/amd64"}, {platform: "linux/arm64"}}
		assert.NoError(t, firstBuildError(builds))
	})

	t.Run("returns the first error in order", func(t *testing.T) {
		wantErr := assert.AnError
		builds := []platformBuild{
			{platform: "linux/amd64"},
			{platform: "linux/arm64", err: wantErr},
			{platform: "linux/riscv64", err: assert.AnError},
		}

		assert.Equal(t, wantErr, firstBuildError(builds))
	})
}
