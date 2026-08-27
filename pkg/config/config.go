package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

var CompressionMethod string

type ImageBuild struct {
	Concurrency  int `json:"concurrency" yaml:"concurrency"`
	HistoryLimit int `json:"historyLimit" yaml:"historyLimit"`
	Interval     int `json:"interval" yaml:"interval"`
}

type Controller struct {
	Logging   Logging   `json:"logging" yaml:"logging"`
	Manager   Manager   `json:"manager" yaml:"manager"`
	Buildkit  Buildkit  `json:"buildkit" yaml:"buildkit"`
	Messaging Messaging `json:"messaging" yaml:"messaging"`
	NewRelic  NewRelic  `json:"newRelic" yaml:"newRelic"`
}

func (c Controller) Validate() error {
	var errs []string

	if c.Manager.ImageBuild.Concurrency < 1 {
		errs = append(errs, "manager.imageBuild.concurrency must be greater than or equal to 1")
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

	seenPoolNames := make(map[string]bool, len(c.Buildkit.PlatformPools))
	for idx, pool := range c.Buildkit.PlatformPools {
		prefix := fmt.Sprintf("buildkit.platformPools[%d]", idx)

		switch {
		case pool.Name == "":
			errs = append(errs, prefix+".name cannot be blank")
		case seenPoolNames[pool.Name]:
			errs = append(errs, fmt.Sprintf("%s.name %q is duplicated", prefix, pool.Name))
		default:
			seenPoolNames[pool.Name] = true
		}
		if pool.Namespace == "" {
			errs = append(errs, prefix+".namespace cannot be blank")
		}
		if pool.PodLabels == nil {
			errs = append(errs, prefix+".podLabels cannot be nil")
		}
		if pool.StatefulSetName == "" {
			errs = append(errs, prefix+".statefulSetName cannot be blank")
		}
		if pool.ServiceName == "" {
			errs = append(errs, prefix+".serviceName cannot be blank")
		}
		if len(pool.Platforms) == 0 {
			errs = append(errs, prefix+".platforms must contain at least 1 entry")
		}

		seenPlatforms := make(map[string]bool, len(pool.Platforms))
		for pIdx, platform := range pool.Platforms {
			pprefix := fmt.Sprintf("%s.platforms[%d]", prefix, pIdx)

			norm, err := hephv1.NormalizePlatform(platform)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s %q is invalid: %s", pprefix, platform, err.Error()))
				continue
			}
			if seenPlatforms[norm] {
				errs = append(errs, fmt.Sprintf("%s %q is duplicated", pprefix, platform))
				continue
			}
			seenPlatforms[norm] = true
		}
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
	HealthProbeAddr      string     `json:"healthProbeAddr" yaml:"healthProbeAddr"`
	MetricsAddr          string     `json:"metricsAddr" yaml:"metricsAddr"`
	WebhookPort          int        `json:"webhookPort" yaml:"webhookPort"`
	WatchNamespaces      []string   `json:"watchNamespaces" yaml:"watchNamespaces,omitempty"`
	EnableLeaderElection bool       `json:"enableLeaderElection" yaml:"enableLeaderElection"`
	ImageBuild           ImageBuild `json:"imageBuild" yaml:"imageBuild"`
}

// Buildkit communication and discovery configuration.
type Buildkit struct {
	// Namespace where the StatefulSet is deployed.
	Namespace string `json:"namespace" yaml:"namespace"`
	// PodLabels assigned to pods by the StatefulSet.
	PodLabels map[string]string `json:"podLabels" yaml:"podLabels"`
	// DaemonPort used to communicate with buildkitd over gRPC.
	DaemonPort int32 `json:"daemonPort" yaml:"daemonPort"`
	// ServiceName for the headless service.
	ServiceName string `json:"serviceName" yaml:"serviceName"`
	// StatefulSetName for the supervising workload.
	StatefulSetName string `json:"statefulSetName" yaml:"statefulSetName"`
	// PoolSyncWaitTime controls how often the worker pool is reconciled.
	PoolSyncWaitTime *time.Duration `json:"poolSyncWaitTime" yaml:"poolSyncWaitTime"`
	// PoolMaxIdleTime controls how long a pod will be allowed to remain unleased before it's terminated.
	PoolMaxIdleTime *time.Duration `json:"poolMaxIdleTime" yaml:"poolMaxIdleTime"`
	// PoolEndpointWatchTimeout is the time limit used when waiting for new pods to become "ready" for traffic.
	PoolEndpointWatchTimeout *int64 `json:"poolEndpointWatchTimeout" yaml:"poolEndpointWatchTimeout"`
	// MTLS parameters.
	MTLS *BuildkitMTLS `json:"mtls,omitempty" yaml:"mtls,omitempty"`
	// Global secrets provided to buildkitd during the build process for all image builds.
	Secrets map[string]string `json:"secrets" yaml:"secrets,omitempty"`
	// Registries parameters.
	Registries map[string]RegistryConfig `json:"registries,omitempty" yaml:"registries,omitempty"`
	// FetchAndExtractTimeout used when processing the remote Docker context tarball.
	// Fetch retries have a hard timeout limit of 4.25 mins because, come on, don't be ridiculous.
	FetchAndExtractTimeout time.Duration `json:"fetchAndExtractTimeout" yaml:"fetchAndExtractTimeout"`
	// PlatformPools declares one or more named buildkit pools and the platforms each serves, natively
	// or via emulation set up out-of-band on their nodes. When empty, a single pool named "default" is
	// synthesized from the Namespace/PodLabels/StatefulSetName/ServiceName fields above, preserving the
	// pre-multi-arch behavior of this config exactly.
	PlatformPools []PlatformPool `json:"platformPools,omitempty" yaml:"platformPools,omitempty"`
}

// PlatformPool describes a single buildkit worker pool and the platforms it can serve.
type PlatformPool struct {
	// Name uniquely identifies this pool.
	Name string `json:"name" yaml:"name"`
	// Namespace where the StatefulSet is deployed.
	Namespace string `json:"namespace" yaml:"namespace"`
	// PodLabels assigned to pods by the StatefulSet.
	PodLabels map[string]string `json:"podLabels" yaml:"podLabels"`
	// StatefulSetName for the supervising workload.
	StatefulSetName string `json:"statefulSetName" yaml:"statefulSetName"`
	// ServiceName for the headless service.
	ServiceName string `json:"serviceName" yaml:"serviceName"`
	// Platforms this pool can serve, in "os/arch[/variant]" syntax (e.g. "linux/amd64").
	Platforms []string `json:"platforms" yaml:"platforms"`
}

// defaultPlatformPoolName is used for the single pool synthesized from the legacy flat
// Namespace/PodLabels/StatefulSetName/ServiceName fields when PlatformPools is unset.
const defaultPlatformPoolName = "default"

// defaultPlatformPoolPlatform is the platform declared for the synthesized default pool: the
// native architecture every pre-multi-arch deployment already runs on. Additional platforms,
// native or emulated, require migrating to PlatformPools (see docs/building.md).
const defaultPlatformPoolPlatform = "linux/amd64"

// Pools returns the configured platform pools, synthesizing a single "default" pool from the legacy
// flat fields when PlatformPools is empty. This is what guarantees an upgrade with no values changes
// behaves identically to the pre-multi-arch single-pool topology.
func (b Buildkit) Pools() []PlatformPool {
	if len(b.PlatformPools) > 0 {
		return b.PlatformPools
	}

	return []PlatformPool{
		{
			Name:            defaultPlatformPoolName,
			Namespace:       b.Namespace,
			PodLabels:       b.PodLabels,
			StatefulSetName: b.StatefulSetName,
			ServiceName:     b.ServiceName,
			Platforms:       []string{defaultPlatformPoolPlatform},
		},
	}
}

// PlatformCapabilities builds the webhook/dispatcher-facing capability set from Pools(), the single
// source of truth for which platforms are servable by this configuration.
func (b Buildkit) PlatformCapabilities() hephv1.PlatformCapabilities {
	poolPlatforms := make(map[string][]string)
	for _, pool := range b.Pools() {
		poolPlatforms[pool.Name] = pool.Platforms
	}

	return hephv1.NewPlatformCapabilities(poolPlatforms)
}

// RegistryConfig options used to relax registry push/pull restrictions.
type RegistryConfig struct {
	// Insecure will allow self-signed certificates.
	Insecure bool `json:"insecure,omitempty" yaml:"insecure,omitempty"`
	// HTTP will allow non-TLS connections.
	HTTP bool `json:"http,omitempty" yaml:"http,omitempty"`
}

// BuildkitMTLS server configuration.
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

func (m *AMQPMessaging) MarshalJSON() ([]byte, error) {
	amqpMessaging := *m
	u, err := url.Parse(amqpMessaging.URL)
	if err != nil {
		return nil, err
	}

	amqpMessaging.URL = u.Redacted()
	return json.Marshal(amqpMessaging)
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
	f, err := os.Open(filename)
	if err != nil {
		return Controller{}, err
	}
	defer f.Close()

	var cfg Controller
	switch ext := filepath.Ext(filename); ext {
	case ".yaml", ".yml":
		decoder := yaml.NewDecoder(f)
		decoder.KnownFields(true)
		for {
			if err = decoder.Decode(&cfg); err == io.EOF {
				return cfg, nil
			} else if err != nil {
				return Controller{}, err
			}
		}
	case ".json":
		decoder := json.NewDecoder(f)
		decoder.DisallowUnknownFields()
		for {
			if err = decoder.Decode(&cfg); err == io.EOF {
				return cfg, nil
			} else if err != nil {
				return Controller{}, err
			}
		}
	default:
		return Controller{}, fmt.Errorf("file extension %q is not allowed", ext)
	}
}

func validatePort(port int) error {
	if port < 1024 || port > 65535 {
		return fmt.Errorf("port %d must be between 1024 and 65535", port)
	}

	return nil
}
