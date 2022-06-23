package buildkit

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/console"
	"github.com/go-logr/logr"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildctl/build"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/dominodatalab/hephaestus/pkg/buildkit/archive"
)

var clientCheckBackoff = wait.Backoff{ // retries after 500ms 1s 2s 4s 8s with jitter
	Steps:    5,
	Duration: 500 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

type clientBuilder struct {
	addr             string
	dockerAuthConfig string
	log              logr.Logger
	bkOpts           []bkclient.ClientOpt
}

func ClientBuilder(addr string) *clientBuilder {
	return &clientBuilder{addr: addr, log: logr.Discard()}
}

func (b *clientBuilder) WithDockerAuthConfig(configDir string) *clientBuilder {
	b.dockerAuthConfig = configDir
	return b
}

func (b *clientBuilder) WithMTLSAuth(caPath, certPath, keyPath string) *clientBuilder {
	u, err := url.Parse(b.addr)
	if err != nil {
		b.log.Error(err, "Cannot parse hostname, skipping mTLS auth", "addr", b.addr)
	} else {
		b.bkOpts = append(b.bkOpts, bkclient.WithCredentials(u.Hostname(), caPath, certPath, keyPath))
	}

	return b
}

func (b *clientBuilder) WithLogger(log logr.Logger) *clientBuilder {
	b.log = log
	return b
}

func (b *clientBuilder) Build(ctx context.Context) (*Client, error) {
	bk, err := bkclient.New(ctx, b.addr, append(b.bkOpts, bkclient.WithFailFast())...)
	if err != nil {
		return nil, fmt.Errorf("failed to create buildkit client: %w", err)
	}

	var lastErr error

	b.log.Info("Confirming buildkitd connectivity")
	err = wait.ExponentialBackoffWithContext(ctx, clientCheckBackoff, func() (done bool, err error) {
		if _, lastErr = bk.ListWorkers(ctx); lastErr != nil {
			b.log.V(1).Info("Buildkitd is not ready")
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to contact buildkitd after %d attempts: %w", clientCheckBackoff.Steps, lastErr)
	}
	b.log.Info("Buildkitd connectivity established")

	return &Client{
		bk:               bk,
		log:              b.log,
		dockerAuthConfig: b.dockerAuthConfig,
	}, nil
}

type BuildOptions struct {
	Context                  string
	ContextDir               string
	Images                   []string
	BuildArgs                []string
	NoCache                  bool
	ImportCache              []string
	DisableInlineCacheExport bool
	Secrets                  map[string]string
}

type Buildkit interface {
	Build(ctx context.Context, opts BuildOptions) error
	Cache(ctx context.Context, image string) error
}

type Client struct {
	bk               *bkclient.Client
	log              logr.Logger
	dockerAuthConfig string
}

func (c *Client) Build(ctx context.Context, opts BuildOptions) error {
	// setup build directory
	buildDir, err := os.MkdirTemp("", "hephaestus-build-")
	if err != nil {
		return fmt.Errorf("failed to create build dir: %w", err)
	}

	defer func(path string) {
		if err := os.RemoveAll(path); err != nil {
			c.log.Error(err, "Failed to delete build context")
		}
	}(buildDir)

	// process build context
	var contentsDir string

	if fi, err := os.Stat(opts.ContextDir); err == nil && fi.IsDir() {
		c.log.Info("Using context dir", "dir", opts.ContextDir)
		contentsDir = opts.ContextDir
	} else {
		c.log.Info("Fetching remote context", "url", opts.Context)
		extract, err := archive.FetchAndExtract(c.log, ctx, opts.Context, buildDir, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("cannot fetch remote context: %w", err)
		}

		contentsDir = extract.ContentsDir
	}
	c.log.V(1).Info("Context extracted", "dir", contentsDir)

	// verify manifest is present
	dockerfile := filepath.Join(contentsDir, "Dockerfile")
	if _, err := os.Stat(dockerfile); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("build requires a Dockerfile inside context dir: %w", err)
	}

	if l := c.log.V(1); l.Enabled() {
		bs, err := os.ReadFile(dockerfile)
		if err != nil {
			return fmt.Errorf("cannot read Dockerfile: %w", err)

		}
		l.Info("Dockerfile contents:\n" + string(bs))
	}

	// Do not cache these as the file contents can change
	// over time (e.g. when mounted from a configmap)
	secrets := make(map[string][]byte)
	for name, path := range opts.Secrets {
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		secrets[name] = contents
	}

	// build solve options
	solveOpt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{},
		LocalDirs: map[string]string{
			"context":    contentsDir,
			"dockerfile": contentsDir,
		},
		Session: []session.Attachable{
			NewDockerAuthProvider(c.dockerAuthConfig),
			secretsprovider.FromMap(secrets),
		},
		CacheExports: []bkclient.CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
	}

	if opts.NoCache {
		solveOpt.FrontendAttrs["no-cache"] = ""
	}

	if opts.DisableInlineCacheExport {
		solveOpt.CacheExports = nil
	}

	for _, ref := range opts.ImportCache {
		solveOpt.CacheImports = []bkclient.CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": ref,
				},
			},
		}
	}

	if len(opts.BuildArgs) != 0 {
		var args []string
		for _, arg := range opts.BuildArgs {
			args = append(args, fmt.Sprintf("build-arg:%s", arg))
		}

		attrs, err := build.ParseOpt(args, nil)
		if err != nil {
			return fmt.Errorf("cannot parse build args: %w", err)
		}

		for k, v := range attrs {
			solveOpt.FrontendAttrs[k] = v
		}
	}

	for _, name := range opts.Images {
		solveOpt.Exports = append(solveOpt.Exports, bkclient.ExportEntry{
			Type: bkclient.ExporterImage,
			Attrs: map[string]string{
				"name": name,
				"push": "true",
			},
		})
	}

	// build/push images
	return c.runSolve(ctx, solveOpt)
}

func (c *Client) Cache(ctx context.Context, image string) error {
	return c.solveWith(ctx, func(buildDir string, solveOpt *bkclient.SolveOpt) error {
		dockerfile := filepath.Join(buildDir, "Dockerfile")
		contents := []byte(fmt.Sprintf("FROM %s\nRUN echo extract\n", image))
		if err := os.WriteFile(dockerfile, contents, 0644); err != nil {
			return fmt.Errorf("failed to create dockerfile: %w", err)
		}

		solveOpt.LocalDirs = map[string]string{
			"context":    buildDir,
			"dockerfile": buildDir,
		}
		solveOpt.Exports = []bkclient.ExportEntry{
			{
				Type: bkclient.ExporterOCI,
				Output: func(m map[string]string) (io.WriteCloser, error) {
					return DiscardCloser{io.Discard}, nil
				},
			},
		}

		return nil
	})
}

func (c *Client) Prune() error {
	c.log.Info("Prune not implemented")

	return nil
}

func (c *Client) solveWith(ctx context.Context, modify func(buildDir string, solveOpt *bkclient.SolveOpt) error) error {
	buildDir, err := os.MkdirTemp("", "hephaestus-build-")
	if err != nil {
		return fmt.Errorf("failed to create build dir: %w", err)
	}
	defer func(path string) {
		if err := os.RemoveAll(path); err != nil {
			c.log.Error(err, "Failed to delete build context")
		}
	}(buildDir)

	solveOpt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{},
		Session: []session.Attachable{
			NewDockerAuthProvider(c.dockerAuthConfig),
		},
	}

	if err = modify(buildDir, &solveOpt); err != nil {
		return err
	}

	return c.runSolve(ctx, solveOpt)
}

func (c *Client) runSolve(ctx context.Context, so bkclient.SolveOpt) error {
	lw := &LogWriter{Logger: c.log}
	ch := make(chan *bkclient.SolveStatus)
	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		var c console.Console
		if cn, err := console.ConsoleFromFile(os.Stderr); err != nil {
			c = cn
		}

		// this operation should return cleanly when solve returns (either by itself or when cancelled) so there's no
		// need to cancel it explicitly. see https://github.com/moby/buildkit/pull/1721 for details.
		_, err := progressui.DisplaySolveStatus(context.Background(), "", c, lw, ch)

		return err
	})

	eg.Go(func() error {
		if _, err := c.bk.Solve(ctx, nil, so, ch); err != nil {
			return err
		}

		c.log.Info("Solve complete")
		return nil
	})

	if err := eg.Wait(); err != nil {
		c.log.Info(fmt.Sprintf("Build failed: %s", err.Error()))
		return fmt.Errorf("buildkit solve issue: %w", err)
	}

	return nil
}
