package buildkit

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	authutil "github.com/containerd/containerd/remotes/docker/auth"
	remoteserrors "github.com/containerd/containerd/remotes/errors"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/gofrs/flock"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/util/progress/progresswriter"
	"golang.org/x/crypto/nacl/sign"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewDockerAuthProvider was pulled directly from the buildkit source code and
// modified to accept a config directory.
//
// The Docker library exposes a couple of functions for loading authentication
// credentials but buildkit only leverages a method that pulls them from the
// DOCKER_CONFIG environment variable. Rather than setting in-proc envvars, I
// will submit an issue/PR with buildkit to see if they can include this option
// as well.
func NewDockerAuthProvider(configDir string) session.Attachable {
	cf, err := config.Load(configDir)
	if err != nil {
		panic(err)
	}

	return &authProvider{
		config:      cf,
		seeds:       &tokenSeeds{dir: config.Dir()},
		loggerCache: map[string]struct{}{},
	}
}

type authProvider struct {
	config      *configfile.ConfigFile
	seeds       *tokenSeeds
	logger      progresswriter.Logger
	loggerCache map[string]struct{}

	// The need for this mutex is not well understood.
	// Without it, the docker cli on OS X hangs when
	// reading credentials from docker-credential-osxkeychain.
	// See issue https://github.com/docker/cli/issues/1862
	mu sync.Mutex
}

func (ap *authProvider) SetLogger(l progresswriter.Logger) {
	ap.mu.Lock()
	ap.logger = l
	ap.mu.Unlock()
}

func (ap *authProvider) Register(server *grpc.Server) {
	auth.RegisterAuthServer(server, ap)
}

func (ap *authProvider) FetchToken(ctx context.Context, req *auth.FetchTokenRequest) (rr *auth.FetchTokenResponse, err error) {
	creds, err := ap.credentials(req.Host)
	if err != nil {
		return nil, err
	}

	to := authutil.TokenOptions{
		Realm:    req.Realm,
		Service:  req.Service,
		Scopes:   req.Scopes,
		Username: creds.Username,
		Secret:   creds.Secret,
	}

	if creds.Secret != "" {
		done := func(progresswriter.SubLogger) error {
			return err
		}
		defer func() {
			err = fmt.Errorf("failed to fetch oauth token: %w", err)
		}()
		ap.mu.Lock()
		name := fmt.Sprintf("[auth] %v token for %s", strings.Join(trimScopePrefix(req.Scopes), " "), req.Host)
		if _, ok := ap.loggerCache[name]; !ok {
			progresswriter.Wrap(name, ap.logger, done)
		}
		ap.mu.Unlock()
		// try GET first because Docker Hub does not support POST
		// switch once support has landed
		resp, err := authutil.FetchToken(ctx, http.DefaultClient, nil, to)
		if err != nil {
			var errStatus remoteserrors.ErrUnexpectedStatus
			if errors.As(err, &errStatus) {
				// retry with POST request
				// As of September 2017, GCR is known to return 404.
				// As of February 2018, JFrog Artifactory is known to return 401.
				if (errStatus.StatusCode == 405 && to.Username != "") || errStatus.StatusCode == 404 || errStatus.StatusCode == 401 {
					resp, err := authutil.FetchTokenWithOAuth(ctx, http.DefaultClient, nil, "buildkit-client", to)
					if err != nil {
						return nil, err
					}

					return toTokenResponse(resp.AccessToken, resp.IssuedAt, resp.ExpiresIn), nil
				}
			}
			return nil, err
		}
		return toTokenResponse(resp.Token, resp.IssuedAt, resp.ExpiresIn), nil
	}
	// do request anonymously
	resp, err := authutil.FetchToken(ctx, http.DefaultClient, nil, to)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch anonymous token: %w", err)
	}
	return toTokenResponse(resp.Token, resp.IssuedAt, resp.ExpiresIn), nil
}

func (ap *authProvider) credentials(host string) (*auth.CredentialsResponse, error) {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if host == "registry-1.docker.io" {
		host = "https://index.docker.io/v1/"
	}
	ac, err := ap.config.GetAuthConfig(host)
	if err != nil {
		return nil, err
	}
	res := &auth.CredentialsResponse{}
	if ac.IdentityToken != "" {
		res.Secret = ac.IdentityToken
	} else {
		res.Username = ac.Username
		res.Secret = ac.Password
	}
	return res, nil
}

func (ap *authProvider) Credentials(ctx context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	resp, err := ap.credentials(req.Host)
	if err != nil || resp.Secret != "" {
		ap.mu.Lock()
		defer ap.mu.Unlock()
		_, ok := ap.loggerCache[req.Host]
		ap.loggerCache[req.Host] = struct{}{}
		if !ok {
			return resp, progresswriter.Wrap(fmt.Sprintf("[auth] sharing credentials for %s", req.Host), ap.logger, func(progresswriter.SubLogger) error {
				return err
			})
		}
	}
	return resp, err
}

func (ap *authProvider) GetTokenAuthority(ctx context.Context, req *auth.GetTokenAuthorityRequest) (*auth.GetTokenAuthorityResponse, error) {
	key, err := ap.getAuthorityKey(req.Host, req.Salt)
	if err != nil {
		return nil, err
	}

	return &auth.GetTokenAuthorityResponse{PublicKey: key[32:]}, nil
}

func (ap *authProvider) VerifyTokenAuthority(ctx context.Context, req *auth.VerifyTokenAuthorityRequest) (*auth.VerifyTokenAuthorityResponse, error) {
	key, err := ap.getAuthorityKey(req.Host, req.Salt)
	if err != nil {
		return nil, err
	}

	priv := new([64]byte)
	copy((*priv)[:], key)

	return &auth.VerifyTokenAuthorityResponse{Signed: sign.Sign(nil, req.Payload, priv)}, nil
}

func (ap *authProvider) getAuthorityKey(host string, salt []byte) (ed25519.PrivateKey, error) {
	if v, err := strconv.ParseBool(os.Getenv("BUILDKIT_NO_CLIENT_TOKEN")); err == nil && v {
		return nil, status.Errorf(codes.Unavailable, "client side tokens disabled")
	}

	creds, err := ap.credentials(host)
	if err != nil {
		return nil, err
	}
	seed, err := ap.seeds.getSeed(host)
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, salt)
	if creds.Secret != "" {
		mac.Write(seed)
	}

	sum := mac.Sum(nil)

	return ed25519.NewKeyFromSeed(sum[:ed25519.SeedSize]), nil
}

func toTokenResponse(token string, issuedAt time.Time, expires int) *auth.FetchTokenResponse {
	resp := &auth.FetchTokenResponse{
		Token:     token,
		ExpiresIn: int64(expires),
	}
	if !issuedAt.IsZero() {
		resp.IssuedAt = issuedAt.Unix()
	}
	return resp
}

func trimScopePrefix(scopes []string) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = strings.TrimPrefix(s, "repository:")
	}
	return out
}

type tokenSeeds struct {
	mu  sync.Mutex
	dir string
	m   map[string]seed
}

type seed struct {
	Seed []byte
}

func (ts *tokenSeeds) getSeed(host string) ([]byte, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if err := os.MkdirAll(ts.dir, 0755); err != nil {
		return nil, err
	}

	if ts.m == nil {
		ts.m = map[string]seed{}
	}

	l := flock.New(filepath.Join(ts.dir, ".token_seed.lock"))
	if err := l.Lock(); err != nil {
		if !errors.Is(err, syscall.EROFS) && !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
	} else {
		defer l.Unlock()
	}

	fp := filepath.Join(ts.dir, ".token_seed")

	// we include client side randomness to avoid chosen plaintext attack from the daemon side
	dt, err := ioutil.ReadFile(fp)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
	} else {
		// ignore error in case of crash during previous marshal
		_ = json.Unmarshal(dt, &ts.m)
	}
	v, ok := ts.m[host]
	if !ok {
		v = seed{Seed: newSeed()}
	}

	ts.m[host] = v

	dt, err = json.MarshalIndent(ts.m, "", "  ")
	if err != nil {
		return nil, err
	}

	if err := ioutil.WriteFile(fp, dt, 0600); err != nil {
		if !errors.Is(err, syscall.EROFS) && !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
	}
	return v.Seed, nil
}

func newSeed() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	return b
}
