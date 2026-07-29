package config

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLoadFromFile(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		expected := genConfig()

		jbs, err := json.Marshal(expected)
		require.NoError(t, err)

		ybs, err := yaml.Marshal(expected)
		require.NoError(t, err)

		for ext, bs := range map[string][]byte{"yaml": ybs, "yml": ybs, "json": jbs} {
			file := createTempFile(t, bs, ext)
			actual, err := LoadFromFile(file.Name())
			require.NoError(t, err)

			assert.Equal(t, expected, actual)
		}
	})

	t.Run("extra config", func(t *testing.T) {
		file := createTempFile(t, []byte("extra: true"), "yaml")
		_, err := LoadFromFile(file.Name())
		if err == nil {
			t.Error("expected an error")
		}
		file = createTempFile(t, []byte(`{"extra": true}`), "json")
		_, err = LoadFromFile(file.Name())
		if err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("bad_format", func(t *testing.T) {
		for _, ext := range []string{"yaml", "yml", "json"} {
			file := createTempFile(t, []byte("01010101010101"), ext)

			_, err := LoadFromFile(file.Name())
			assert.Error(t, err)
		}
	})

	t.Run("bad_extension", func(t *testing.T) {
		config := genConfig()
		bs, err := yaml.Marshal(config)
		require.NoError(t, err)

		file := createTempFile(t, bs, "foo")

		_, err = LoadFromFile(file.Name())
		assert.Error(t, err)
	})

	t.Run("missing_file", func(t *testing.T) {
		_, err := LoadFromFile("missing")
		assert.Error(t, err)
	})
}

func TestControllerValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		config := genConfig()
		assert.NoError(t, config.Validate())
	})

	t.Run("bad_health_probe_addr", func(t *testing.T) {
		config := genConfig()
		config.Manager.HealthProbeAddr = ""
		assert.Error(t, config.Validate())
	})

	t.Run("bad_metrics_addr", func(t *testing.T) {
		config := genConfig()
		config.Manager.MetricsAddr = ""
		assert.Error(t, config.Validate())
	})

	t.Run("bad_buildkit_labels", func(t *testing.T) {
		config := genConfig()
		config.Buildkit.PodLabels = nil
		assert.Error(t, config.Validate())
	})

	t.Run("bad_buildkit_namespace", func(t *testing.T) {
		config := genConfig()
		config.Buildkit.Namespace = ""
		assert.Error(t, config.Validate())
	})

	t.Run("bad_buildkit_daemon_port", func(t *testing.T) {
		config := genConfig()
		badPorts := []int32{-5000, 80, 66_000}
		for _, port := range badPorts {
			config.Buildkit.DaemonPort = port
			assert.Error(t, config.Validate())
		}
	})

	t.Run("bad_image_build_concurrency", func(t *testing.T) {
		config := genConfig()
		for _, n := range []int{0, -5} {
			config.Manager.ImageBuild.Concurrency = n
			assert.Error(t, config.Validate())
		}
	})

	t.Run("bad_new_relic", func(t *testing.T) {
		config := genConfig()

		config.NewRelic.Enabled = true
		assert.Error(t, config.Validate())

		config.NewRelic.LicenseKey = "0123456789012345678901234567890123456789"
		assert.NoError(t, config.Validate())
	})

	t.Run("valid_platform_pools", func(t *testing.T) {
		config := genConfig()
		config.Buildkit.PlatformPools = []PlatformPool{
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
		}
		assert.NoError(t, config.Validate())
	})

	t.Run("bad_platform_pools", func(t *testing.T) {
		base := func() PlatformPool {
			return PlatformPool{
				Name:            "amd64",
				Namespace:       "test-ns",
				PodLabels:       map[string]string{"app": "buildkit-amd64"},
				StatefulSetName: "buildkit-amd64",
				ServiceName:     "buildkit-amd64",
				Platforms:       []string{"linux/amd64"},
			}
		}

		mutations := map[string]func(*PlatformPool){
			"blank_name":              func(p *PlatformPool) { p.Name = "" },
			"blank_namespace":         func(p *PlatformPool) { p.Namespace = "" },
			"nil_pod_labels":          func(p *PlatformPool) { p.PodLabels = nil },
			"blank_stateful_set_name": func(p *PlatformPool) { p.StatefulSetName = "" },
			"blank_service_name":      func(p *PlatformPool) { p.ServiceName = "" },
			"no_platforms":            func(p *PlatformPool) { p.Platforms = nil },
		}

		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				config := genConfig()
				pool := base()
				mutate(&pool)
				config.Buildkit.PlatformPools = []PlatformPool{pool}
				assert.Error(t, config.Validate())
			})
		}

		t.Run("duplicate_name", func(t *testing.T) {
			config := genConfig()
			config.Buildkit.PlatformPools = []PlatformPool{base(), base()}
			assert.Error(t, config.Validate())
		})
	})
}

func TestBuildkitPools(t *testing.T) {
	t.Run("synthesizes default pool when unconfigured", func(t *testing.T) {
		buildkit := Buildkit{
			Namespace:       "test-ns",
			PodLabels:       map[string]string{"app": "buildkit"},
			StatefulSetName: "buildkit",
			ServiceName:     "buildkit",
		}

		pools := buildkit.Pools()
		require.Len(t, pools, 1)
		assert.Equal(t, "default", pools[0].Name)
		assert.Equal(t, buildkit.Namespace, pools[0].Namespace)
		assert.Equal(t, buildkit.PodLabels, pools[0].PodLabels)
		assert.Equal(t, buildkit.StatefulSetName, pools[0].StatefulSetName)
		assert.Equal(t, buildkit.ServiceName, pools[0].ServiceName)
	})

	t.Run("returns configured pools verbatim when present", func(t *testing.T) {
		configured := []PlatformPool{
			{Name: "amd64", Platforms: []string{"linux/amd64"}},
			{Name: "arm64", Platforms: []string{"linux/arm64"}},
		}
		buildkit := Buildkit{
			Namespace:     "test-ns",
			PlatformPools: configured,
		}

		assert.Equal(t, configured, buildkit.Pools())
	})
}

func TestBuildkitWithPool(t *testing.T) {
	buildkit := Buildkit{
		Namespace:       "shared-ns",
		PodLabels:       map[string]string{"app": "buildkit"},
		StatefulSetName: "buildkit",
		ServiceName:     "buildkit",
		DaemonPort:      1234,
		Secrets:         map[string]string{"npmrc": "/secrets/npmrc"},
	}

	pool := PlatformPool{
		Name:            "arm64",
		Namespace:       "arm64-ns",
		PodLabels:       map[string]string{"app": "buildkit-arm64"},
		StatefulSetName: "buildkit-arm64",
		ServiceName:     "buildkit-arm64",
		Platforms:       []string{"linux/arm64"},
	}

	result := buildkit.WithPool(pool)

	assert.Equal(t, pool.Namespace, result.Namespace)
	assert.Equal(t, pool.PodLabels, result.PodLabels)
	assert.Equal(t, pool.StatefulSetName, result.StatefulSetName)
	assert.Equal(t, pool.ServiceName, result.ServiceName)

	// pool-agnostic settings are preserved unchanged
	assert.Equal(t, buildkit.DaemonPort, result.DaemonPort)
	assert.Equal(t, buildkit.Secrets, result.Secrets)

	// original is untouched
	assert.Equal(t, "shared-ns", buildkit.Namespace)
}

func TestBuildkitPlatformCapabilities(t *testing.T) {
	t.Run("no capabilities when unconfigured", func(t *testing.T) {
		buildkit := Buildkit{Namespace: "test-ns", PodLabels: map[string]string{"app": "buildkit"}}

		caps := buildkit.PlatformCapabilities()
		assert.False(t, caps.Supports("linux/amd64"))
	})

	t.Run("reflects configured pools", func(t *testing.T) {
		buildkit := Buildkit{
			PlatformPools: []PlatformPool{
				{Name: "amd64", Platforms: []string{"linux/amd64"}},
				{Name: "arm64", Platforms: []string{"linux/arm64"}},
			},
		}

		caps := buildkit.PlatformCapabilities()
		assert.True(t, caps.Supports("linux/amd64"))
		assert.True(t, caps.Supports("linux/arm64"))
		assert.False(t, caps.Supports("linux/arm/v7"))
	})
}

func TestSensitiveDataRedaction(t *testing.T) {
	config := Controller{
		Messaging: Messaging{
			AMQP: &AMQPMessaging{
				URL: "amqp://username:password@server:5672",
			},
		},
	}

	data, err := json.Marshal(config)
	assert.NoError(t, err)

	var actual Controller
	require.NoError(t, json.Unmarshal(data, &actual))

	assert.Equal(t, "amqp://username:password@server:5672", config.Messaging.AMQP.URL)
	assert.Equal(t, "amqp://username:xxxxx@server:5672", actual.Messaging.AMQP.URL)
}

func createTempFile(t *testing.T, contents []byte, ext string) *os.File {
	t.Helper()

	file, err := os.CreateTemp("", fmt.Sprintf("config.*.%s", ext))
	require.NoError(t, err)

	t.Cleanup(func() { os.Remove(file.Name()) })

	_, err = file.Write(contents)
	require.NoError(t, err)

	require.NoError(t, file.Close())

	return file
}

func genConfig() Controller {
	return Controller{
		Buildkit: Buildkit{
			PodLabels: map[string]string{
				"app": "buildkit",
			},
			Namespace:  "test-ns",
			DaemonPort: 1234,
		},
		Manager: Manager{
			HealthProbeAddr: "5000",
			MetricsAddr:     "6000",
			WebhookPort:     8443,
			ImageBuild:      ImageBuild{Concurrency: 1},
		},
	}
}
