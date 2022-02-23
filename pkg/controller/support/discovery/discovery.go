package discovery

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dominodatalab/controller-util/collection"
	"github.com/dominodatalab/controller-util/core"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

// BuildkitEndpoints searches for a headless Endpoint object with the provided
// labels and returns a slice of buildkit connection strings for every address.
func BuildkitEndpoints(ctx *core.Context, cfg config.Buildkit) ([]string, error) {
	labels := collection.MergeStringMaps(cfg.Labels, map[string]string{"service.kubernetes.io/headless": ""})

	var epList corev1.EndpointsList
	listOpts := []client.ListOption{
		client.InNamespace(cfg.Namespace),
		client.MatchingLabels(labels),
	}
	if err := ctx.Client.List(ctx, &epList, listOpts...); err != nil {
		return nil, fmt.Errorf("cannot retrieve endpoints in ns %q with labels %v: %w", cfg.Namespace, cfg.Labels, err)
	}
	if len(epList.Items) != 1 {
		return nil, fmt.Errorf("expected a single endpoint, got %d", len(epList.Items))
	}
	ep := epList.Items[0]

	var addrs []string
	for _, subset := range ep.Subsets {
		var portDefined bool
		for _, port := range subset.Ports {
			if port.Port == cfg.DaemonPort {
				portDefined = true
				break
			}
		}
		if !portDefined {
			return nil, fmt.Errorf("endpoint %q does not expose daemon port %d", ep.Name, cfg.DaemonPort)
		}

		for _, addr := range subset.Addresses {
			addrs = append(addrs, buildAddrStr(cfg.DaemonPort, addr.TargetRef.Name, ep.Name, ep.Namespace))
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("no buildkit endpoints found using %+v", cfg)
	}

	return addrs, nil
}

// BuildkitService searches for a ClusterIP Service object with the provided
// labels and returns a buildkit connection string.
func BuildkitService(ctx *core.Context, cfg config.Buildkit) (string, error) {
	var svcList corev1.ServiceList
	listOpts := []client.ListOption{
		client.InNamespace(cfg.Namespace),
		client.MatchingLabels(cfg.Labels),
	}
	if err := ctx.Client.List(ctx, &svcList, listOpts...); err != nil {
		return "", err
	}
	if len(svcList.Items) == 0 {
		return "", errors.New("cannot find buildkit service")
	}

	var addr string
	for _, svc := range svcList.Items {
		if svc.Spec.Type == corev1.ServiceTypeClusterIP {
			addr = buildAddrStr(cfg.DaemonPort, svc.Name, svc.Namespace)
		}
	}
	if addr == "" {
		return "", fmt.Errorf("buildkid clusterIP service not found with labels %v", cfg.Labels)
	}

	return addr, nil
}

func buildAddrStr(port int32, names ...string) string {
	dns := strings.Join(names, ".")
	return fmt.Sprintf("tcp://%s:%d", dns, port)
}
