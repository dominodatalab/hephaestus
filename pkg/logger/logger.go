package logger

import (
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

var (
	consoleEncoder zapcore.Encoder
	jsonEncoder    zapcore.Encoder
)

func New(cfg config.Logging) (logr.Logger, error) {
	// container logging
	var containerEncoder zapcore.Encoder
	enc := strings.ToLower(cfg.Container.Encoder)

	if enc == "" || enc == "console" {
		containerEncoder = consoleEncoder
	} else if enc == "json" {
		containerEncoder = jsonEncoder
	} else {
		return logr.Logger{}, fmt.Errorf("%q is an invalid encoder", enc)
	}

	ll, err := parseLevel(cfg.Container.LogLevel)
	if err != nil {
		return logr.Logger{}, fmt.Errorf("invalid container log level: %w", err)
	}

	cores := []zapcore.Core{
		zapcore.NewCore(&ctrlzap.KubeAwareEncoder{Encoder: containerEncoder}, zapcore.Lock(os.Stdout), ll),
	}

	// logfile logging
	if cfg.Logfile.Enabled {
		file, err := os.Create(cfg.Logfile.Filepath)
		if err != nil {
			return logr.Logger{}, fmt.Errorf("cannot create logfile logger: %w", err)
		}

		level, err := parseLevel(cfg.Logfile.LogLevel)
		if err != nil {
			return logr.Logger{}, fmt.Errorf("invalid logfile log level: %w", err)
		}
		fileCore := zapcore.NewCore(&ctrlzap.KubeAwareEncoder{Encoder: jsonEncoder}, zapcore.Lock(file), level)

		cores = append(cores, fileCore)
	}

	// process options, join cores and construct a logger
	sl, err := parseLevel(cfg.StacktraceLevel)
	if err != nil {
		return logr.Logger{}, fmt.Errorf("invalid stacktrace log level: %w", err)
	}

	opts := []zap.Option{
		zap.AddCallerSkip(1),
		zap.AddStacktrace(sl),
		zap.ErrorOutput(zapcore.Lock(os.Stderr)),
	}
	log := zap.New(zapcore.NewTee(cores...), opts...)

	// adapt interface
	return zapr.NewLogger(log), nil
}

func parseLevel(name string) (zapcore.LevelEnabler, error) {
	lvl := zap.NewAtomicLevel()
	if err := lvl.UnmarshalText([]byte(name)); err != nil {
		return nil, fmt.Errorf("%q is an invalid log level: %w", name, err)
	}

	return lvl, nil
}

func init() {
	humanCfg := zap.NewDevelopmentEncoderConfig()
	machineCfg := zap.NewProductionEncoderConfig()

	humanCfg.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	machineCfg.EncodeTime = zapcore.RFC3339NanoTimeEncoder

	consoleEncoder = zapcore.NewConsoleEncoder(humanCfg)
	jsonEncoder = zapcore.NewJSONEncoder(machineCfg)
}
