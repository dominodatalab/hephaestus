package credentials

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	typesregistry "github.com/moby/moby/api/types/registry"
)

const (
	registryProbeTimeout = 15 * time.Second

	// dockerHubEndpoint is where Docker Hub serves the v2 API. The legacy
	// credential keys (index.docker.io, docker.io) all point here.
	dockerHubEndpoint = "registry-1.docker.io"
)

func loginHost(serverAddress string) (string, error) {
	if serverAddress == "" {
		return dockerHubEndpoint, nil
	}

	if !strings.HasPrefix(serverAddress, "https://") && !strings.HasPrefix(serverAddress, "http://") {
		serverAddress = "https://" + serverAddress
	}

	u, err := url.Parse(serverAddress)
	if err != nil {
		return "", fmt.Errorf("unable to parse server address: %w", err)
	}

	switch strings.ToLower(u.Hostname()) {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return dockerHubEndpoint, nil
	}

	return u.Host, nil
}

var errPlainHTTP = errors.New("registry is not configured as insecure, refusing plain http")

// go-containerregistry falls back to HTTP for localhost and private IPs. Block
// that fallback so credentials only travel in clear text with explicit consent.
type httpsOnlyTransport struct{ http.RoundTripper }

func (t httpsOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, errPlainHTTP
	}

	return t.RoundTripper.RoundTrip(req)
}

// A 401 must satisfy errdefs.IsUnauthorized so Verify stops retrying rejected
// credentials. Other responses must remain distinct from network failures.
func authenticate(
	ctx context.Context,
	auth *typesregistry.AuthConfig,
	insecureRegistries []string,
	userAgent string,
) error {
	host, err := loginHost(auth.ServerAddress)
	if err != nil {
		return err
	}

	var opts []name.Option
	insecure := slices.Contains(insecureRegistries, host)
	if insecure {
		opts = append(opts, name.Insecure)
	}

	reg, err := name.NewRegistry(host, opts...)
	if err != nil {
		return err
	}

	base, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("unexpected default transport %T", remote.DefaultTransport)
	}

	base = base.Clone()
	defer base.CloseIdleConnections()

	var inner http.RoundTripper = httpsOnlyTransport{base}
	if insecure {
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		inner = base
	}

	ctx, cancel := context.WithTimeout(ctx, registryProbeTimeout)
	defer cancel()

	rt, err := transport.NewWithContext(ctx, reg, authn.FromConfig(authn.AuthConfig{
		Username:      auth.Username,
		Password:      auth.Password,
		Auth:          auth.Auth,
		IdentityToken: auth.IdentityToken,
	}), transport.NewUserAgent(inner, userAgent), nil)
	if err != nil {
		return classifyAuthError(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+reg.RegistryStr()+"/v2/", http.NoBody)
	if err != nil {
		return err
	}

	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return classifyAuthError(err)
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return classifyAuthError(err)
	}

	return nil
}

// When both schemes fail, a refused HTTPS attempt must not make a registry's
// HTTP response look unreachable to Verify.
func classifyAuthError(err error) error {
	te, ok := errors.AsType[*transport.Error](err)
	switch {
	case !ok:
		return err
	case te.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", errdefs.ErrUnauthenticated, te)
	default:
		return te
	}
}
