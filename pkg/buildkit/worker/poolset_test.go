package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

func TestNewPoolSet(t *testing.T) {
	t.Run("synthesizes a single default pool when unconfigured", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()
		cfg := config.Buildkit{
			Namespace:       "test-ns",
			PodLabels:       map[string]string{"app": "buildkit"},
			StatefulSetName: "buildkit",
			ServiceName:     "buildkit",
		}

		ps := NewPoolSet(fakeClient, cfg)

		pool, ok := ps.Get("default")
		assert.True(t, ok)
		assert.NotNil(t, pool)

		_, ok = ps.Get("nonexistent")
		assert.False(t, ok)
	})

	t.Run("builds one pool per configured platform pool", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()
		cfg := config.Buildkit{
			PlatformPools: []config.PlatformPool{
				{
					Name:            "amd64",
					Namespace:       "test-ns",
					PodLabels:       map[string]string{"app": "buildkit-amd64"},
					StatefulSetName: "buildkit-amd64",
					ServiceName:     "buildkit-amd64",
					Platforms:       []string{"linux/amd64"},
				},
				{
					Name:            "arm64",
					Namespace:       "test-ns",
					PodLabels:       map[string]string{"app": "buildkit-arm64"},
					StatefulSetName: "buildkit-arm64",
					ServiceName:     "buildkit-arm64",
					Platforms:       []string{"linux/arm64"},
				},
			},
		}

		ps := NewPoolSet(fakeClient, cfg)

		for _, name := range []string{"amd64", "arm64"} {
			pool, ok := ps.Get(name)
			assert.True(t, ok, "expected pool %q", name)
			assert.NotNil(t, pool)
		}

		_, ok := ps.Get("riscv64")
		assert.False(t, ok)
	})
}
