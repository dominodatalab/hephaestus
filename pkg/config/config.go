package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

type Controller struct {
	Logging   Logging   `json:"logging" yaml:"logging"`
	Manager   Manager   `json:"manager" yaml:"manager"`
	Buildkit  Buildkit  `json:"buildkit" yaml:"buildkit"`
	Messaging Messaging `json:"messaging" yaml:"messaging"`
	NewRelic  NewRelic  `json:"newRelic" yaml:"newRelic"`

	ImageBuildMaxConcurrency int `json:"imageBuildMaxConcurrency" yaml:"imageBuildMaxConcurrency"`
}

func (c Controller) Validate() error {
	var errs []string

	if c.ImageBuildMaxConcurrency < 1 {
		errs = append(errs, "imageBuildMaxConcurrency must be greater than or equal to 1")
	}
	if c.Manager.HealthProbeAddr == "" {
		errs = append(errs, "manager.healthProbeAddr cannot be blank")
	}
	if c.Manager.MetricsAddr == "" {
		errs = append(errs, "manager.metricsAddr cannot be blank")
	}
	if err := validatePort(c.Manager.WebhookPort); err != nil {
		errs = append(errs, fmt.Sprintf("manager.webhookPort is invalid: %s", err.Error()))
	}

	if c.Buildkit.PodLabels == nil {
		errs = append(errs, "buildkit.podLabels cannot be nil")
	}
	if c.Buildkit.Namespace == "" {
		errs = append(errs, "buildkit.namespace cannot be blank")
	}
	if err := validatePort(int(c.Buildkit.DaemonPort)); err != nil {
		errs = append(errs, fmt.Sprintf("buildkit.daemonPort is invalid: %s", err.Error()))
	}

	if c.NewRelic.Enabled && c.NewRelic.LicenseKey == "" {
		errs = append(errs, "newRelic.licenseKey cannot be blank")
	}

	if len(errs) != 0 {
		return fmt.Errorf("config is invalid: %s", strings.Join(errs, ", "))
	}

	return nil
}

type ContainerLogging struct {
	Encoder  string `json:"encoder" yaml:"encoder"`
	LogLevel string `json:"level" yaml:"level"`
}

type LogfileLogging struct {
	Enabled  bool   `json:"enabled" yaml:"enabled"`
	Filepath string `json:"filepath" yaml:"filepath"`
	LogLevel string `json:"level" yaml:"level"`
}

type Logging struct {
	StacktraceLevel string `json:"stacktraceLevel" yaml:"stacktraceLevel"`

	Container ContainerLogging `json:"container" yaml:"container"`
	Logfile   LogfileLogging   `json:"logfile" yaml:"logfile"`
}

type Manager struct {
	HealthProbeAddr      string   `json:"healthProbeAddr" yaml:"healthProbeAddr"`
	MetricsAddr          string   `json:"metricsAddr" yaml:"metricsAddr"`
	WebhookPort          int      `json:"webhookPort" yaml:"webhookPort"`
	WatchNamespaces      []string `json:"watchNamespaces" yaml:"watchNamespaces,omitempty"`
	EnableLeaderElection bool     `json:"enableLeaderElection" yaml:"enableLeaderElection"`
}

type Buildkit struct {
	Namespace       string            `json:"namespace" yaml:"namespace"`
	PodLabels       map[string]string `json:"podLabels" yaml:"podLabels"`
	DaemonPort      int32             `json:"daemonPort" yaml:"daemonPort"`
	ServiceName     string            `json:"serviceName" yaml:"serviceName"`
	StatefulSetName string            `json:"statefulSetName" yaml:"statefulSetName"`

	Secrets map[string]string `json:"secrets" yaml:"secrets,omitempty"`

	Registries map[string]RegistryConfig `json:"registries,omitempty" yaml:"registries,omitempty"`

	PoolSyncWaitTime *time.Duration `json:"poolSyncWaitTime" yaml:"poolSyncWaitTime"`
	PoolMaxIdleTime  *time.Duration `json:"poolMaxIdleTime" yaml:"poolMaxIdleTime"`
	PoolWatchTimeout *int64         `json:"poolWatchTimeout" yaml:"poolWatchTimeout"`

	MTLS *BuildkitMTLS `json:"mtls,omitempty" yaml:"mtls,omitempty"`
}

type RegistryConfig struct {
	Insecure bool `json:"insecure,omitempty" yaml:"insecure,omitempty"`
	HTTP     bool `json:"http,omitempty" yaml:"http,omitempty"`
}

type BuildkitMTLS struct {
	CACertPath string `json:"caCertPath" yaml:"caCertPath"`
	CertPath   string `json:"certPath" yaml:"certPath"`
	KeyPath    string `json:"keyPath" yaml:"keyPath"`
}

type Messaging struct {
	Enabled bool            `json:"enabled" yaml:"enabled"`
	AMQP    *AMQPMessaging  `json:"amqp" yaml:"amqp"`
	Kafka   *KafkaMessaging `json:"kafka" yaml:"kafka"`
}

type AMQPMessaging struct {
	URL      string `json:"url" yaml:"url"`
	Exchange string `json:"exchange" yaml:"exchange"`
	Queue    string `json:"queue" yaml:"queue"`
}

type KafkaMessaging struct {
	Servers   []string `json:"servers" yaml:"servers"`
	Topic     string   `json:"topic" yaml:"topic"`
	Partition string   `json:"partition" yaml:"partition"`
}

type NewRelic struct {
	Enabled    bool              `json:"enabled" yaml:"enabled"`
	AppName    string            `json:"appName" yaml:"appName"`
	Labels     map[string]string `json:"labels" yaml:"labels,omitempty"`
	LicenseKey string            `json:"licenseKey" yaml:"licenseKey"`
}

func LoadFromFile(filename string) (Controller, error) {
	bs, err := os.ReadFile(filename)
	if err != nil {
		return Controller{}, err
	}

	var cfg Controller
	switch ext := filepath.Ext(filename); ext {
	case ".yaml", ".yml":
		err = yaml.Unmarshal(bs, &cfg)
	case ".json":
		err = json.Unmarshal(bs, &cfg)
	default:
		return Controller{}, fmt.Errorf("file extension %q is not allowed", ext)
	}

	return cfg, err
}

func validatePort(port int) error {
	if port < 1024 || port > 65535 {
		return fmt.Errorf("port %d must be between 1024 and 65535", port)
	}

	return nil
}
