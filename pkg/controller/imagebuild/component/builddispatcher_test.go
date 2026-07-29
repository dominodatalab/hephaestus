package component

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

func TestCheckSinglePoolServesPlatforms(t *testing.T) {
	t.Run("empty platforms is always fine", func(t *testing.T) {
		c := &BuildDispatcherComponent{cfg: config.Buildkit{}}
		assert.NoError(t, c.checkSinglePoolServesPlatforms(nil))
	})

	t.Run("single pool covering every requested platform", func(t *testing.T) {
		c := &BuildDispatcherComponent{
			cfg: config.Buildkit{
				PlatformPools: []config.PlatformPool{
					{Name: "emulated", Platforms: []string{"linux/amd64", "linux/arm64"}},
				},
			},
		}
		assert.NoError(t, c.checkSinglePoolServesPlatforms([]string{"linux/amd64", "linux/arm64"}))
	})

	t.Run("platforms spanning multiple pools fails clearly", func(t *testing.T) {
		c := &BuildDispatcherComponent{
			cfg: config.Buildkit{
				PlatformPools: []config.PlatformPool{
					{Name: "amd64-native", Platforms: []string{"linux/amd64"}},
					{Name: "arm64-native", Platforms: []string{"linux/arm64"}},
				},
			},
		}
		err := c.checkSinglePoolServesPlatforms([]string{"linux/amd64", "linux/arm64"})
		assert.Error(t, err)
	})

	t.Run("unresolvable platform fails", func(t *testing.T) {
		c := &BuildDispatcherComponent{
			cfg: config.Buildkit{
				PlatformPools: []config.PlatformPool{
					{Name: "amd64-native", Platforms: []string{"linux/amd64"}},
				},
			},
		}
		err := c.checkSinglePoolServesPlatforms([]string{"linux/riscv64"})
		assert.Error(t, err)
	})
}
