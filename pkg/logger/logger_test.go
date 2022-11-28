package logger

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

func TestNewZap(t *testing.T) {
	t.Run("container_encoders", func(t *testing.T) {
		_, err := NewZap(config.Logging{Container: config.ContainerLogging{Encoder: "console"}})
		assert.NoError(t, err)

		_, err = NewZap(config.Logging{Container: config.ContainerLogging{Encoder: "json"}})
		assert.NoError(t, err)

		_, err = NewZap(config.Logging{Container: config.ContainerLogging{Encoder: "steve"}})
		assert.EqualError(t, err, `"steve" is an invalid encoder`)
	})

	validLogLevels := []string{"debug", "info", "warn", "error", "dpanic", "panic", "fatal"}

	t.Run("container_log_levels", func(t *testing.T) {
		for _, level := range validLogLevels {
			_, err := NewZap(config.Logging{Container: config.ContainerLogging{LogLevel: level}})
			assert.NoError(t, err)
		}

		_, err := NewZap(config.Logging{Container: config.ContainerLogging{LogLevel: "steve"}})
		assert.EqualError(t, err, `invalid container log config: unrecognized level: "steve"`)
	})

	t.Run("logfile_log_levels", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "log-test.json")
		conf := config.Logging{Logfile: config.LogfileLogging{Enabled: true, Filepath: fp}}

		for _, level := range validLogLevels {
			conf.Logfile.LogLevel = level
			_, err := NewZap(conf)
			assert.NoError(t, err)
		}

		conf.Logfile.LogLevel = "steve"
		_, err := NewZap(conf)
		assert.EqualError(t, err, `invalid logfile log config: unrecognized level: "steve"`)
	})

	t.Run("logfile_path", func(t *testing.T) {
		conf := config.Logging{Logfile: config.LogfileLogging{Enabled: true}}

		_, err := NewZap(conf)
		assert.EqualError(t, err, "cannot create logfile logger: open : no such file or directory")

		logfile := filepath.Join(t.TempDir(), "log-test.json")
		conf.Logfile.Filepath = logfile

		zaplog, err := NewZap(conf)
		require.NoError(t, err)

		zaplog.Info("hello steve")
		bs, err := os.ReadFile(logfile)
		require.NoError(t, err)

		assert.Contains(t, string(bs), `"msg":"hello steve"`)
	})

	t.Run("stacktrace_levels", func(t *testing.T) {
		for _, level := range validLogLevels {
			_, err := NewZap(config.Logging{StacktraceLevel: level})
			assert.NoError(t, err)
		}

		_, err := NewZap(config.Logging{StacktraceLevel: "steve"})
		assert.EqualError(t, err, `invalid stacktrace log config: unrecognized level: "steve"`)
	})
}
