package logger

import (
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

func New(cfg config.Logging) (logr.Logger, error) {
	opts := []ctrlzap.Opts{
		ctrlzap.UseDevMode(cfg.Development),
		func(opts *ctrlzap.Options) { opts.TimeEncoder = zapcore.RFC3339NanoTimeEncoder },
	}

	if cfg.Encoder != "" {
		switch strings.ToLower(cfg.Encoder) {
		case "console":
			opts = append(opts, ctrlzap.ConsoleEncoder())
		case "json":
			opts = append(opts, ctrlzap.JSONEncoder())
		default:
			return logr.Logger{}, fmt.Errorf("invalid logger encoder %q", cfg.Encoder)
		}
	}

	if cfg.LogLevel != "" {
		lvl := zap.NewAtomicLevel()
		if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
			return logr.Logger{}, fmt.Errorf("cannot set logger level: %w", err)
		}
		opts = append(opts, ctrlzap.Level(lvl))
	}

	if cfg.StacktraceLevel != "" {
		lvl := zap.NewAtomicLevel()
		if err := lvl.UnmarshalText([]byte(cfg.StacktraceLevel)); err != nil {
			return logr.Logger{}, fmt.Errorf("cannot set logger stacktrace level: %w", err)
		}
		opts = append(opts, ctrlzap.StacktraceLevel(lvl))
	}

	return ctrlzap.New(opts...), nil
}
