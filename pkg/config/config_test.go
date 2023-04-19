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

	t.Run("bad_image_build_max_concurrency", func(t *testing.T) {
		config := genConfig()
		for _, n := range []int{0, -5} {
			config.ImageBuildMaxConcurrency = n
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
		ImageBuildMaxConcurrency: 1,
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
		},
	}
}
