package buildkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildctl/build"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/dominodatalab/hephaestus/pkg/buildkit/archive"
	hephconfig "github.com/dominodatalab/hephaestus/pkg/config"
)

var clientCheckBackoff = wait.Backoff{ // retries after 500ms 1s 2s 4s 8s 16s 32s 64s with jitter
	Duration: 500 * time.Millisecond,
	Factor:   2.0,
	Steps:    8,
	Jitter:   0.1,
}

type ClientBuilder struct {
	addr            string
	dockerConfigDir string
	log             logr.Logger
	bkOpts          []bkclient.ClientOpt
}

func NewClientBuilder(addr string) *ClientBuilder {
	return &ClientBuilder{addr: addr, log: logr.Discard()}
}

func (b *ClientBuilder) WithDockerConfigDir(configDir string) *ClientBuilder {
	b.dockerConfigDir = configDir
	return b
}

func (b *ClientBuilder) WithMTLSAuth(caPath, certPath, keyPath string) *ClientBuilder {
	u, err := url.Parse(b.addr)
	if err != nil {
		b.log.Error(err, "Cannot parse hostname, skipping mTLS auth", "addr", b.addr)
	} else {
		b.bkOpts = append(b.bkOpts,
			bkclient.WithServerConfig(u.Hostname(), caPath),
			bkclient.WithCredentials(certPath, keyPath),
		)
	}

	return b
}

func (b *ClientBuilder) WithLogger(log logr.Logger) *ClientBuilder {
	b.log = log
	return b
}

func (b *ClientBuilder) Build(ctx context.Context) (*Client, error) {
	bk, err := bkclient.New(ctx, b.addr, b.bkOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create buildkit client: %w", err)
	}

	var lastErr error

	b.log.Info("Confirming buildkitd connectivity")
	err = wait.ExponentialBackoffWithContext(ctx, clientCheckBackoff, func(ctx context.Context) (done bool, err error) {
		if _, lastErr = bk.ListWorkers(ctx); lastErr != nil {
			b.log.V(1).Info("Buildkitd is not ready")

			//nolint:nilerr // returning and err here will stop the loop immediately
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to contact buildkitd after %d attempts: %w", clientCheckBackoff.Steps, lastErr)
	}
	b.log.Info("Buildkitd connectivity established")

	return &Client{
		bk:              bk,
		log:             b.log,
		dockerConfigDir: b.dockerConfigDir,
	}, nil
}

type BuildOptions struct {
	Context                  string
	ContextDir               string
	DockerfileContents       string
	Images                   []string
	BuildArgs                []string
	NoCache                  bool
	ImportCache              []string
	DisableInlineCacheExport bool
	Secrets                  map[string]string
	SecretsData              map[string][]byte
	FetchAndExtractTimeout   time.Duration
}

type Buildkit interface {
	Build(ctx context.Context, opts BuildOptions) error
	Cache(ctx context.Context, image string) error
}

type Client struct {
	bk              *bkclient.Client
	log             logr.Logger
	dockerConfigDir string
}

func validateCompression(compression string, name string) map[string]string {
	attrs := make(map[string]string)
	attrs["name"] = name
	const truth = "true"
	switch compression {
	case "estargz":
		attrs["push"] = truth
		attrs["compression"] = "estargz"
		attrs["force-compression"] = truth
		attrs["oci-mediatypes"] = truth
	case "zstd":
		attrs["compression"] = "zstd"
		attrs["force-compression"] = truth
		attrs["push"] = truth
	// default is gzip
	default:
		attrs["push"] = truth
	}
	return attrs
}

func (c *Client) Build(ctx context.Context, opts BuildOptions) (string, error) {
	// setup build directory
	buildDir, err := os.MkdirTemp("", "hephaestus-build-")
	if err != nil {
		return "", fmt.Errorf("failed to create build dir: %w", err)
	}

	defer func(path string) {
		if err := os.RemoveAll(path); err != nil {
			c.log.Error(err, "Failed to delete build context")
		}
	}(buildDir)

	dockerConfig, err := config.Load(c.dockerConfigDir)
	if err != nil {
		c.log.Error(err, "Error loading config file")
	}

	// process build context
	var contentsDir string
	fi, err := os.Stat(opts.ContextDir)
	switch {
	case err == nil && fi.IsDir():
		c.log.Info("Using context dir", "dir", opts.ContextDir)
		contentsDir = opts.ContextDir
	case strings.TrimSpace(opts.Context) != "":
		c.log.Info("Fetching remote context", "url", opts.Context)
		extract, extractErr := archive.FetchAndExtract(ctx, c.log, opts.Context, buildDir, opts.FetchAndExtractTimeout)
		if extractErr != nil {
			return "", fmt.Errorf("cannot fetch remote context: %w", err)
		}
		contentsDir = extract.ContentsDir
	case strings.TrimSpace(opts.DockerfileContents) != "":
		c.log.Info("Creating context from DockerfileContents")
		contentsDir, err = os.MkdirTemp(buildDir, "dockerfile-contents-")
		if err != nil {
			return "", fmt.Errorf("cannot create temp directory for dockerfileContents: %w", err)
		}
		err = os.WriteFile(path.Join(contentsDir, "Dockerfile"), []byte(opts.DockerfileContents), os.FileMode(0644))
		if err != nil {
			return "", fmt.Errorf("cannot write temporary file for dockerfileContents: %w", err)
		}
	default:
		return "", errors.New("no valid docker context provided")
	}
	c.log.V(1).Info("Context extracted", "dir", contentsDir)

	// verify manifest is present
	dockerfile := filepath.Join(contentsDir, "Dockerfile")
	if _, err := os.Stat(dockerfile); errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("build requires a Dockerfile inside context dir: %w", err)
	}

	if l := c.log.V(1); l.Enabled() {
		bs, err := os.ReadFile(dockerfile)
		if err != nil {
			return "", fmt.Errorf("cannot read Dockerfile: %w", err)
		}
		l.Info("Dockerfile contents:\n" + string(bs))
	}

	// Do not cache these as the file contents can change
	// over time (e.g. when mounted from a configmap)
	secrets := make(map[string][]byte)
	for name, path := range opts.Secrets {
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}

		secrets[name] = contents
	}

	// merge in preloaded data
	for name, contents := range opts.SecretsData {
		secrets[name] = contents
	}

	for k := range secrets {
		c.log.Info("Found secret", "key", k)
	}

	contentsFS, err := fsutil.NewFS(contentsDir)
	if err != nil {
		return "", fmt.Errorf("unable to create context dir: %w", err)
	}

	// build solve options
	solveOpt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{},
		LocalMounts: map[string]fsutil.FS{
			"context":    contentsFS,
			"dockerfile": contentsFS,
		},
		Session: []session.Attachable{
			authprovider.NewDockerAuthProvider(dockerConfig, nil),
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

		attrs, err := build.ParseOpt(args)
		if err != nil {
			return "", fmt.Errorf("cannot parse build args: %w", err)
		}

		for k, v := range attrs {
			solveOpt.FrontendAttrs[k] = v
		}
	}
	for _, name := range opts.Images {
		bkclientattrs := validateCompression(hephconfig.CompressionMethod, name)
		solveOpt.Exports = append(solveOpt.Exports, bkclient.ExportEntry{
			Type:  bkclient.ExporterImage,
			Attrs: bkclientattrs,
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

		buildFS, err := fsutil.NewFS(buildDir)
		if err != nil {
			return fmt.Errorf("failed to create build dir: %w", err)
		}

		solveOpt.LocalMounts = map[string]fsutil.FS{
			"context":    buildFS,
			"dockerfile": buildFS,
		}
		solveOpt.Exports = []bkclient.ExportEntry{
			{
				Type: bkclient.ExporterOCI,
				Output: func(_ map[string]string) (io.WriteCloser, error) {
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

	dockerConfig, err := config.Load(c.dockerConfigDir)
	if err != nil {
		c.log.Error(err, "Error loading config file")
	}
	solveOpt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{},
		Session: []session.Attachable{
			authprovider.NewDockerAuthProvider(dockerConfig, nil),
		},
	}

	if err = modify(buildDir, &solveOpt); err != nil {
		return err
	}

	_, err = c.runSolve(ctx, solveOpt)
	return err
}

func (c *Client) ResolveAuth(registryHostname string) (authn.Authenticator, error) {
	cf, err := config.Load(c.dockerConfigDir)
	if err != nil {
		return nil, err
	}
	cfg, err := cf.GetAuthConfig(registryHostname)
	if err != nil {
		return nil, err
	}

	return authn.FromConfig(authn.AuthConfig{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Auth:          cfg.Auth,
		IdentityToken: cfg.IdentityToken,
		RegistryToken: cfg.RegistryToken,
	}), nil
}

func (c *Client) runSolve(ctx context.Context, so bkclient.SolveOpt) (string, error) {
	lw := &LogWriter{Logger: c.log}
	ch := make(chan *bkclient.SolveStatus)
	eg, ctx := errgroup.WithContext(ctx)

	d, err := progressui.NewDisplay(lw, progressui.PlainMode)
	if err != nil {
		return "", fmt.Errorf("unable to setup buildkit logging: %w", err)
	}

	//nolint:contextcheck
	eg.Go(func() error {
		// this operation should return cleanly when solve returns (either by itself or when cancelled) so there's no
		// need to cancel it explicitly. see https://github.com/moby/buildkit/pull/1721 for details.
		_, err = d.UpdateFrom(context.Background(), ch)
		return err
	})

	var imageName string

	eg.Go(func() error {
		res, err := c.bk.Solve(ctx, nil, so, ch)
		if err != nil {
			return err
		}

		c.log.Info("Solve complete")
		imageName = res.ExporterResponse["image.name"]

		return nil
	})

	if err := eg.Wait(); err != nil {
		c.log.Info(fmt.Sprintf("Build failed: %s", err.Error()))
		return "", fmt.Errorf("buildkit solve issue: %w", err)
	}

	c.log.Info(fmt.Sprintf("Final image name: %s", imageName))
	return imageName, nil
}
