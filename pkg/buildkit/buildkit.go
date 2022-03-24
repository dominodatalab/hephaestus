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
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/progress/progressui"
	"golang.org/x/sync/errgroup"

	"github.com/dominodatalab/hephaestus/pkg/buildkit/archive"
)

type clientBuilder struct {
	ctx              context.Context
	addr             string
	dockerAuthConfig string
	log              logr.Logger
	bkOpts           []bkclient.ClientOpt
}

func ClientBuilder(ctx context.Context, addr string) *clientBuilder {
	return &clientBuilder{ctx: ctx, addr: addr, log: logr.Discard()}
}

func (b *clientBuilder) WithDockerAuthConfig(configDir string) *clientBuilder {
	b.dockerAuthConfig = configDir
	return b
}

func (b *clientBuilder) WithMTLSAuth(caPath, certPath, keyPath string) *clientBuilder {
	u, err := url.Parse(b.addr)
	if err != nil {
		b.log.Error(err, "Cannot parse hostname, kipping mTLS auth", "addr", b.addr)
	} else {
		b.bkOpts = append(b.bkOpts, bkclient.WithCredentials(u.Hostname(), caPath, certPath, keyPath))
	}

	return b
}

func (b *clientBuilder) WithLogger(log logr.Logger) *clientBuilder {
	b.log = log
	return b
}

func (b *clientBuilder) Build() (*Client, error) {
	bk, err := bkclient.New(b.ctx, b.addr, append(b.bkOpts, bkclient.WithFailFast())...)
	if err != nil {
		return nil, fmt.Errorf("failed to create buildkit client: %w", err)
	}

	return &Client{
		bk:               bk,
		ctx:              b.ctx,
		log:              b.log,
		dockerAuthConfig: b.dockerAuthConfig,
	}, nil
}

type BuildOptions struct {
	Context    string
	ContextDir string
	Images     []string
	BuildArgs  []string
}

type Buildkit interface {
	Build(opts BuildOptions) error
	Cache(image string) error
}

type Client struct {
	bk               *bkclient.Client
	ctx              context.Context
	log              logr.Logger
	dockerAuthConfig string
}

func (c *Client) Build(opts BuildOptions) error {
	return c.solveWith(func(buildDir string, solveOpt *bkclient.SolveOpt) error {
		var contentsDir string

		if fi, err := os.Stat(opts.ContextDir); err == nil && fi.IsDir() {
			c.log.Info("Using context dir", "dir", opts.ContextDir)
			contentsDir = opts.ContextDir
		} else {
			c.log.Info("Fetching remote context", "url", opts.Context)
			extract, err := archive.FetchAndExtract(c.log, c.ctx, opts.Context, buildDir, 5*time.Minute)
			if err != nil {
				return err
			}

			contentsDir = extract.ContentsDir
		}
		c.log.V(1).Info(contentsDir)

		solveOpt.LocalDirs = map[string]string{
			"context":    contentsDir,
			"dockerfile": contentsDir,
		}

		for _, name := range opts.Images {
			solveOpt.Exports = append(solveOpt.Exports, bkclient.ExportEntry{
				Type: bkclient.ExporterImage,
				Attrs: map[string]string{
					"push": "true",
					"name": name,
				},
			})
		}

		return nil
	})
}

func (c *Client) Cache(image string) error {
	return c.solveWith(func(buildDir string, solveOpt *bkclient.SolveOpt) error {
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

func (c *Client) solveWith(modify func(buildDir string, solveOpt *bkclient.SolveOpt) error) error {
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

	return c.runSolve(solveOpt)
}

func (c *Client) runSolve(so bkclient.SolveOpt) error {
	lw := &LogWriter{Logger: c.log}

	ch := make(chan *bkclient.SolveStatus)
	eg, ctx := errgroup.WithContext(c.ctx)

	eg.Go(func() error {
		resp, err := c.bk.Solve(ctx, nil, so, ch)
		if err != nil {
			return err
		}

		// NOTE: this can be used to limit the size of images pushed to the registry
		c.log.Info("Solve complete", "image.size", resp.ExporterResponse["size"])
		return nil
	})
	eg.Go(func() error {
		var c console.Console
		if cn, err := console.ConsoleFromFile(os.Stderr); err != nil {
			c = cn
		}

		_, err := progressui.DisplaySolveStatus(ctx, "", c, lw, ch)
		return err
	})

	if err := eg.Wait(); err != nil {
		return fmt.Errorf("buildkit solve issue: %w", err)
	}

	return nil
}
