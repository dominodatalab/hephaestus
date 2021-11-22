package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dominodatalab/controller-util/core"
	corev1 "k8s.io/api/core/v1"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/docker"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
)

func LookupEndpoints(ctx *core.Context, cfg config.BuildkitConfig) ([]string, error) {
	if cfg.StaticEndpoints != nil {
		return cfg.StaticEndpoints, nil
	}

	var eps corev1.Endpoints
	if err := ctx.Client.Get(ctx, cfg.DynamicEndpoints.ObjectKey(), &eps); err != nil {
		return nil, fmt.Errorf("cannot retreive endpoints %+v: %w", cfg.DynamicEndpoints, err)
	}

	var addrs []string
	for _, subset := range eps.Subsets {
		var daemonPort int32

		for _, port := range subset.Ports {
			if port.Name == cfg.PortName {
				daemonPort = port.Port
			}
		}
		if daemonPort == 0 {
			return nil, fmt.Errorf("port named %s not defined in endpoints %+v", cfg.PortName, eps)
		}

		for _, addr := range subset.Addresses {
			addrs = append(addrs, fmt.Sprintf("tcp://%s:%d", addr.IP, daemonPort))
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("no builders found using %+v", cfg)
	}

	ctx.Log.Info("Found buildkit builders", "addrs", addrs)
	return addrs, nil
}

func ProcessCredentials(credentials []hephv1.RegistryCredentials) (string, error) {
	configDir, err := os.MkdirTemp("", "hephaestus-docker-config-")
	if err != nil {
		return "", err
	}

	auths := docker.AuthConfigs{}
	for _, cred := range credentials {
		auths[cred.Server] = docker.AuthConfig{
			Username: cred.BasicAuth.Username,
			Password: cred.BasicAuth.Password,
		}
	}
	dockerCfg := docker.Config{Auths: auths}

	configJSON, err := json.Marshal(dockerCfg)
	if err != nil {
		return "", err
	}

	filename := filepath.Join(configDir, "config.json")
	if err = os.WriteFile(filename, configJSON, 0644); err != nil {
		return "", err
	}

	return configDir, nil
}
