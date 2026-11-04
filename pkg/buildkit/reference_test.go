package buildkit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseImageReference(t *testing.T) {
	t.Run("secure registry parses normally", func(t *testing.T) {
		ref, err := ParseImageReference("registry.example.com/repo:v1", nil)
		assert.NoError(t, err)
		assert.Equal(t, "registry.example.com/repo:v1", ref.Name())
	})

	t.Run("insecure registry uses the insecure parse option", func(t *testing.T) {
		ref, err := ParseImageReference("insecure.example.com/repo:v1", []string{"insecure.example.com"})
		assert.NoError(t, err)
		assert.Equal(t, "insecure.example.com/repo:v1", ref.Name())
	})

	t.Run("malformed reference errors", func(t *testing.T) {
		_, err := ParseImageReference("not a valid ref!!", nil)
		assert.Error(t, err)
	})
}
