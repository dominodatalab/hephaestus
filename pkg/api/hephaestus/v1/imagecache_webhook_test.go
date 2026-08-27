package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// admission.WithDefaulter[*ImageCache] binds Default's method receiver to a single static template
// registered once at manager startup (see admission.WithDefaulter's `new` closure) and always calls
// it as template.Default(ctx, obj), where obj is the freshly-decoded per-request object - a distinct
// value from the receiver. A Default() that mutates its receiver instead of obj silently does
// nothing in production, since only obj's mutations reach the admission response. These tests call
// Default the same way to catch that regression, instead of obj.Default(ctx, obj) where receiver and
// param are the same value and a receiver-mutation bug is invisible.
func TestImageCacheDefault(t *testing.T) {
	t.Run("dedupes platforms case-insensitively, like ImageBuild", func(t *testing.T) {
		template := &ImageCache{}
		obj := &ImageCache{Spec: ImageCacheSpec{
			Platforms: []string{"linux/amd64", "LINUX/AMD64", "linux/arm64"},
		}}

		assert.NoError(t, template.Default(context.Background(), obj))
		assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, obj.Spec.Platforms)
		assert.Nil(t, template.Spec.Platforms, "Default must not mutate its receiver")
	})

	t.Run("nil platforms stay nil", func(t *testing.T) {
		template := &ImageCache{}
		obj := &ImageCache{}

		assert.NoError(t, template.Default(context.Background(), obj))
		assert.Nil(t, obj.Spec.Platforms)
	})
}

// admission.WithValidator[*ImageCache] has the same static-receiver-vs-decoded-obj split as
// WithDefaulter (see TestImageCacheDefault) - ValidateCreate/ValidateUpdate must validate the obj
// parameter, not their receiver, or every request is validated against an empty template regardless
// of what was actually submitted.
func TestImageCacheValidate(t *testing.T) {
	template := &ImageCache{}

	t.Run("validates obj, not the receiver: valid obj passes despite an empty receiver", func(t *testing.T) {
		obj := &ImageCache{Spec: ImageCacheSpec{Images: []string{"alpine:3.20"}}}

		_, err := template.ValidateCreate(context.Background(), obj)
		assert.NoError(t, err)
	})

	t.Run("validates obj, not the receiver: invalid obj fails despite an empty receiver", func(t *testing.T) {
		obj := &ImageCache{}

		_, err := template.ValidateCreate(context.Background(), obj)
		assert.Error(t, err)
	})

	t.Run("ValidateUpdate validates the new obj", func(t *testing.T) {
		oldObj := &ImageCache{}
		newObj := &ImageCache{Spec: ImageCacheSpec{Images: []string{"alpine:3.20"}}}

		_, err := template.ValidateUpdate(context.Background(), oldObj, newObj)
		assert.NoError(t, err)
	})
}
