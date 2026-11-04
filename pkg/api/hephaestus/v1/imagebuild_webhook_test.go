package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// admission.WithDefaulter[*ImageBuild] binds Default's method receiver to a single static template
// registered once at manager startup and always calls it as template.Default(ctx, obj), where obj is
// the freshly-decoded per-request object - a distinct value from the receiver. A Default() that
// mutates its receiver instead of obj silently does nothing in production, since only obj's
// mutations reach the admission response. This test calls Default the same way to catch that
// regression, instead of obj.Default(ctx, obj) where receiver and param are the same value and a
// receiver-mutation bug is invisible.
func TestImageBuildDefault(t *testing.T) {
	t.Run("dedupes platforms case-insensitively", func(t *testing.T) {
		template := &ImageBuild{}
		obj := &ImageBuild{Spec: ImageBuildSpec{
			Platforms: []string{"linux/amd64", "LINUX/AMD64", "linux/arm64"},
		}}

		assert.NoError(t, template.Default(context.Background(), obj))
		assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, obj.Spec.Platforms)
		assert.Nil(t, template.Spec.Platforms, "Default must not mutate its receiver")
	})

	t.Run("nil platforms stay nil", func(t *testing.T) {
		template := &ImageBuild{}
		obj := &ImageBuild{}

		assert.NoError(t, template.Default(context.Background(), obj))
		assert.Nil(t, obj.Spec.Platforms)
	})
}
