package credentials

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	"github.com/containerd/errdefs"
	typesregistry "github.com/moby/moby/api/types/registry"
)

// This file checks a credential the way `docker login` does. It ports the
// behaviour of registry.Service.Auth from github.com/docker/docker v28 onto
// containerd's registry authorizer, which buildkit also uses, so the check
// needs neither the frozen docker/docker module nor the beta moby/moby/v2.

const (
	loginUserAgent = "DominoDataLab_Hephaestus/1.0"

	// Docker Hub config keys name the index, but logins go to the registry.
	dockerHubLoginHost = "registry-1.docker.io"

	loginTimeout = 15 * time.Second
)

// defaultInsecureCIDRs are insecure whatever the build config says, as in dockerd.
var defaultInsecureCIDRs = []string{"127.0.0.0/8", "::1/128"}

type loginEndpoint struct {
	url       *url.URL
	tlsConfig *tls.Config
}

// convertToHostname strips an http(s) scheme and any path from a docker config
// server key.
func convertToHostname(server string) string {
	stripped := server
	if rest, ok := strings.CutPrefix(stripped, "http://"); ok {
		stripped = rest
	} else if rest, ok := strings.CutPrefix(stripped, "https://"); ok {
		stripped = rest
	}

	stripped, _, _ = strings.Cut(stripped, "/")

	return stripped
}

// registryLogin checks auth against the registry named by auth.ServerAddress.
//
// HTTPS comes first. Plain HTTP is tried only for an insecure registry, and
// never after a 401: credentials the registry rejected are not resent in clear.
func registryLogin(ctx context.Context, auth typesregistry.AuthConfig, insecureRegistries []string) error {
	endpoints, err := loginEndpoints(ctx, auth.ServerAddress, insecureRegistries)
	if err != nil {
		return err
	}

	var lastErr error
	for _, endpoint := range endpoints {
		err := loginToEndpoint(ctx, auth, endpoint)
		if err == nil {
			return nil
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errdefs.IsUnauthorized(err) {
			return err
		}

		lastErr = err
	}

	return lastErr
}

func loginEndpoints(ctx context.Context, serverAddress string, insecureRegistries []string) ([]loginEndpoint, error) {
	address := serverAddress
	if !strings.HasPrefix(address, "https://") && !strings.HasPrefix(address, "http://") {
		address = "https://" + address
	}

	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("unable to parse server address: %w", err)
	}

	host := u.Host
	if host == "" {
		return nil, fmt.Errorf("server address %q has no host", convertToHostname(serverAddress))
	}

	if host == "docker.io" || host == "index.docker.io" {
		return []loginEndpoint{{
			url:       &url.URL{Scheme: "https", Host: dockerHubLoginHost},
			tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}}, nil
	}

	secure := !isInsecureRegistry(ctx, host, insecureRegistries)
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// An insecure registry is one the build config opted out of verification for.
		InsecureSkipVerify: !secure,
	}

	endpoints := []loginEndpoint{{url: &url.URL{Scheme: "https", Host: host}, tlsConfig: tlsConfig}}
	if !secure {
		endpoints = append(endpoints, loginEndpoint{url: &url.URL{Scheme: "http", Host: host}, tlsConfig: tlsConfig})
	}

	return endpoints, nil
}

// isInsecureRegistry reports whether host is listed by name, or resolves into a
// listed or default CIDR. Entries may carry an http(s) scheme, which is ignored.
func isInsecureRegistry(ctx context.Context, host string, insecureRegistries []string) bool {
	var cidrs []*net.IPNet
	for _, entry := range slices.Concat(insecureRegistries, defaultInsecureCIDRs) {
		lower := strings.ToLower(entry)
		switch {
		case strings.HasPrefix(lower, "http://"):
			entry = entry[len("http://"):]
		case strings.HasPrefix(lower, "https://"):
			entry = entry[len("https://"):]
		}

		if _, ipNet, err := net.ParseCIDR(entry); err == nil {
			cidrs = append(cidrs, ipNet)
			continue
		}

		if entry == host {
			return true
		}
	}

	return hostInCIDRs(ctx, host, cidrs)
}

func hostInCIDRs(ctx context.Context, host string, cidrs []*net.IPNet) bool {
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
	}

	var addresses []net.IP
	if ip := net.ParseIP(hostname); ip != nil {
		addresses = []net.IP{ip}
	} else {
		// A name that does not resolve matches nothing, so it stays secure.
		addresses, err = net.DefaultResolver.LookupIP(ctx, "ip", hostname)
		if err != nil {
			return false
		}
	}

	for _, address := range addresses {
		for _, cidr := range cidrs {
			if cidr.Contains(address) {
				return true
			}
		}
	}

	return false
}

func loginToEndpoint(ctx context.Context, auth typesregistry.AuthConfig, endpoint loginEndpoint) error {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     endpoint.tlsConfig,
			DisableKeepAlives:   true,
		},
		Timeout: loginTimeout,
	}

	authorizer := docker.NewDockerAuthorizer(
		docker.WithAuthClient(client),
		docker.WithAuthHeader(http.Header{"User-Agent": []string{loginUserAgent}}),
		docker.WithAuthCreds(func(string) (string, string, error) {
			// An identity token is an OAuth refresh token. An empty username makes
			// containerd use the refresh grant, as docker does whenever one is set.
			if auth.IdentityToken != "" {
				return "", auth.IdentityToken, nil
			}

			return auth.Username, auth.Password, nil
		}),
	)

	v2URL := endpoint.url.String() + "/v2/"

	// An unauthenticated ping collects the registry's challenge.
	resp, err := loginGet(ctx, client, v2URL, nil)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		// No auth required, so there is no credential to get wrong.
		return nil
	case http.StatusUnauthorized:
		if err := authorizer.AddResponses(ctx, []*http.Response{resp}); err != nil {
			return err
		}
	default:
		return loginStatusError(v2URL, resp.StatusCode)
	}

	authResp, err := loginGet(ctx, client, v2URL, authorizer)
	if err != nil {
		return err
	}
	defer drainAndClose(authResp)

	if authResp.StatusCode != http.StatusOK {
		return loginStatusError(v2URL, authResp.StatusCode)
	}

	return nil
}

// loginGet sends a GET. A non-nil authorizer adds credentials first, and a token
// server's 401 comes back as unauthorized. The caller closes the body.
func loginGet(
	ctx context.Context, client *http.Client, target string, authorizer docker.Authorizer,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", loginUserAgent)

	if authorizer != nil {
		if err := authorizer.Authorize(ctx, req); err != nil {
			var status remoteerrors.ErrUnexpectedStatus
			if errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized {
				return nil, fmt.Errorf("%w: %w", errdefs.ErrUnauthenticated, err)
			}

			return nil, err
		}
	}

	return client.Do(req)
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// loginStatusError reports a registry's answer. A 401 is final; anything else
// may be transient, so Verify retries it.
func loginStatusError(target string, statusCode int) error {
	err := fmt.Errorf("login attempt to %s failed with status: %d %s", target, statusCode, http.StatusText(statusCode))
	if statusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %w", errdefs.ErrUnauthenticated, err)
	}

	return err
}
