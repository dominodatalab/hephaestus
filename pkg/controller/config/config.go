package config

import (
	"flag"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type Config struct {
	Manager  ManagerConfig  `json:"manager" yaml:"manager"`
	Buildkit BuildkitConfig `json:"buildkit" yaml:"buildkit"`
	Logging  LogConfig      `json:"logging" yaml:"logging"`
}

type ManagerConfig struct {
	ProbeAddr            string `json:"probeAddr" yaml:"probeAddr"`
	MetricsAddr          string `json:"metricsAddr" yaml:"metricsAddr"`
	WebhookPort          int    `json:"webhookPort" yaml:"webhookPort"`
	EnableLeaderElection bool   `json:"enableLeaderElection" yaml:"enableLeaderElection"`
}

type BuildkitConfig struct {
	StaticEndpoints  []string              `json:"staticEndpoints" yaml:"staticEndpoints"`
	PortName         string                `json:"portName" yaml:"portName"`
	DynamicEndpoints DynamicEndpointConfig `json:"endpoints" yaml:"endpoints"`
}

type DynamicEndpointConfig struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace" yaml:"namespace"`
}

func (ec DynamicEndpointConfig) ObjectKey() client.ObjectKey {
	return client.ObjectKey{
		Name:      ec.Name,
		Namespace: ec.Namespace,
	}
}

type LogConfig struct {
	Development     bool   `json:"development" yaml:"development"`
	Encoder         string `json:"encoder" yaml:"encoder"`
	LogLevel        string `json:"logLevel" yaml:"logLevel"`
	StacktraceLevel string `json:"stacktraceLevel" yaml:"stacktraceLevel"`
}

func (lc LogConfig) ZapOptions() (*zap.Options, error) {
	// NOTE: there is probably a better way to do this

	fs := &flag.FlagSet{}
	zapOpts := &zap.Options{}
	zapOpts.BindFlags(fs)

	args := []string{fmt.Sprintf("--zap-devel=%t", lc.Development)}
	if lc.Encoder != "" {
		args = append(args, fmt.Sprintf("--zap-encoder=%s", lc.Encoder))
	}
	if lc.LogLevel != "" {
		args = append(args, fmt.Sprintf("--zap-log-level=%s", lc.LogLevel))
	}
	if lc.StacktraceLevel != "" {
		args = append(args, fmt.Sprintf("--zap-stacktrace-level=%s", lc.StacktraceLevel))
	}

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("zap options generation failed: %w", err)
	}

	return zapOpts, nil
}
