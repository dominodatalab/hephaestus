package buildkit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAssembleManifestListsRejectsEmptyRefs(t *testing.T) {
	var c *Client

	_, err := c.AssembleManifestLists(context.Background(), nil, []string{"registry.example.com/repo:v1"}, nil)
	assert.Error(t, err)
}

func TestAssembleManifestListsRejectsEmptyFinalRefs(t *testing.T) {
	var c *Client

	_, err := c.AssembleManifestLists(context.Background(), nil, nil, []PlatformImageRef{{Platform: "linux/amd64", ImageRef: "registry.example.com/repo:v1"}})
	assert.Error(t, err)
}

func TestParsePlatform(t *testing.T) {
	t.Run("os/arch", func(t *testing.T) {
		p, err := parsePlatform("linux/amd64")
		assert.NoError(t, err)
		assert.Equal(t, "linux", p.OS)
		assert.Equal(t, "amd64", p.Architecture)
		assert.Empty(t, p.Variant)
	})

	t.Run("os/arch/variant", func(t *testing.T) {
		p, err := parsePlatform("linux/arm64/v8")
		assert.NoError(t, err)
		assert.Equal(t, "linux", p.OS)
		assert.Equal(t, "arm64", p.Architecture)
		assert.Equal(t, "v8", p.Variant)
	})

	t.Run("invalid", func(t *testing.T) {
		for _, in := range []string{"linux", "", "a/b/c/d"} {
			_, err := parsePlatform(in)
			assert.Error(t, err, "expected error for %q", in)
		}
	})
}
