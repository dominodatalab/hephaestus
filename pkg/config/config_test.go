package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadFromFile(t *testing.T) {
	t.Skip("please write me")
}

func TestGenerateDefaults(t *testing.T) {
	t.Skip("please write me")
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
}

func genConfig() Controller {
	return Controller{
		Manager: Manager{
			HealthProbeAddr: "5000",
			MetricsAddr:     "6000",
			WebhookPort:     8443,
		},
		Buildkit: Buildkit{
			PodLabels: map[string]string{
				"app": "buildkit",
			},
			Namespace:  "test-ns",
			DaemonPort: 1234,
		},
	}
}
