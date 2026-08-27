package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImageCacheDefault(t *testing.T) {
	t.Run("dedupes platforms case-insensitively, like ImageBuild", func(t *testing.T) {
		obj := &ImageCache{Spec: ImageCacheSpec{
			Platforms: []string{"linux/amd64", "LINUX/AMD64", "linux/arm64"},
		}}

		assert.NoError(t, obj.Default(context.Background(), obj))
		assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, obj.Spec.Platforms)
	})

	t.Run("nil platforms stay nil", func(t *testing.T) {
		obj := &ImageCache{}

		assert.NoError(t, obj.Default(context.Background(), obj))
		assert.Nil(t, obj.Spec.Platforms)
	})
}
