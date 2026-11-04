package worker

import (
	"context"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

// PoolSet is a named collection of worker pools, one per platform pool declared in
// config.Buildkit.Pools(). A build whose requested platforms resolve to more than one pool (see
// hephv1.PlatformCapabilities.ResolvePools) leases from each named pool independently.
type PoolSet struct {
	pools map[string]Pool
}

// NewPoolSet builds one AutoscalingPool per config.Buildkit.Pools() entry (or the single
// synthesized "default" pool when PlatformPools is unset), each with the same clientset/options,
// differing only in the worker-identity fields (Namespace/PodLabels/StatefulSetName/ServiceName)
// substituted via config.Buildkit.WithPool. A single configured/synthesized pool reproduces the
// pre-multi-arch single-pool topology exactly.
func NewPoolSet(clientset kubernetes.Interface, cfg config.Buildkit, opts ...PoolOption) PoolSet {
	poolCfgs := cfg.Pools()
	pools := make(map[string]Pool, len(poolCfgs))

	for _, poolCfg := range poolCfgs {
		pools[poolCfg.Name] = NewPool(clientset, cfg.WithPool(poolCfg), opts...)
	}

	return PoolSet{pools: pools}
}

// Get returns the named pool, if configured.
func (ps PoolSet) Get(name string) (Pool, bool) {
	p, ok := ps.pools[name]
	return p, ok
}

// Start runs every pool's control loop concurrently. It returns when all pools have stopped
// cleanly, or as soon as any pool's Start returns an error (which cancels the others via the
// shared errgroup context).
func (ps PoolSet) Start(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)

	for _, pool := range ps.pools {
		eg.Go(func() error { return pool.Start(ctx) })
	}

	return eg.Wait()
}
