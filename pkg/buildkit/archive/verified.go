package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
)

// maxRedirects is the number of redirects DownloadVerified follows before giving up.
const maxRedirects = 10

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

var (
	// ErrChecksumMismatch is returned when the sha256 of the downloaded content differs from the expected one.
	ErrChecksumMismatch = errors.New("sha256 checksum mismatch")
	// ErrSizeExceeded is returned when the download is larger than the configured maximum size.
	ErrSizeExceeded = errors.New("download exceeds maximum size")
	// ErrInsecureTokenTransport is returned when a bearer token is set but the URL would send it in clear text.
	ErrInsecureTokenTransport = errors.New("refusing to send bearer token over an insecure connection")
)

// VerifiedDownloadOptions configures DownloadVerified.
type VerifiedDownloadOptions struct {
	// BearerToken, when not empty, is sent as "Authorization: Bearer <token>". It is never logged. It is only sent
	// over https (plain http is accepted for loopback hosts only, see ErrInsecureTokenTransport) and it is dropped
	// when a redirect leads to a different host or from https to http.
	BearerToken string
	// SHA256 is the expected hex encoded sha256 of the content. Required. The hash is computed over the bytes
	// exactly as they are sent on the wire: response compression is disabled, so a Content-Encoding is never
	// transparently decoded.
	SHA256 string
	// MaxSizeBytes is the maximum number of bytes accepted. Zero or negative means no limit.
	MaxSizeBytes int64
	// Timeout bounds the whole operation: every attempt and the waits between attempts. Zero means no timeout
	// beyond the one carried by the context passed to DownloadVerified.
	Timeout time.Duration
}

// readError tags an error that happened while reading the response body, as opposed to writing the destination
// file or verifying the content. Read errors are transport failures, so they are worth a retry.
type readError struct{ err error }

func (e *readError) Error() string { return "reading response body: " + e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// readTagger wraps a reader so that every failure of the wrapped reader, but the end of the stream, is a readError.
type readTagger struct{ r io.Reader }

func (t readTagger) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &readError{err: err}
	}
	return n, err
}

type verification struct {
	token  string
	sha256 string // lower case hex
	max    int64
}

// save streams the response body to fp, hashing it and enforcing the size cap. The file is removed on any error.
// Failures reading the body are returned as a readError.
func (v *verification) save(resp *http.Response, fp string) (err error) {
	if v.max > 0 && resp.ContentLength > v.max {
		return fmt.Errorf("%w: content length %d is larger than %d bytes", ErrSizeExceeded, resp.ContentLength, v.max)
	}

	out, err := os.Create(fp)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(fp)
		}
	}()

	return v.copyBody(out, resp.Body, resp.ContentLength)
}

// copyBody copies body to out while hashing it, then checks the size cap, the length announced by the server
// (contentLength, negative when unknown) and the checksum. Only failures of body are returned as a readError, so
// that a failing out (a full disk for example) is not mistaken for a transport failure.
func (v *verification) copyBody(out io.Writer, body io.Reader, contentLength int64) error {
	body = readTagger{r: body}
	if v.max > 0 {
		body = io.LimitReader(body, v.max+1) // one extra byte detects an overflow
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), body)
	if err != nil {
		return err
	}
	if v.max > 0 && n > v.max {
		return fmt.Errorf("%w: more than %d bytes received", ErrSizeExceeded, v.max)
	}
	if contentLength >= 0 && n < contentLength {
		return &readError{err: fmt.Errorf("%w: got %d of %d bytes", io.ErrUnexpectedEOF, n, contentLength)}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.sha256 {
		return fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, v.sha256, got)
	}

	return nil
}

// tokenTransportAllowed reports whether a bearer token may be sent to rawURL: only over https, or over plain http
// when the host is loopback (localhost, 127.0.0.0/8 or ::1), where nothing leaves the machine.
func tokenTransportAllowed(rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, fmt.Errorf("invalid download URL: %w", err)
	}

	switch u.Scheme {
	case schemeHTTPS:
		return true, nil
	case schemeHTTP:
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true, nil
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback(), nil
	default:
		return false, nil
	}
}

// keepTokenOnRedirect reports whether the Authorization header may follow a redirect from the original request
// URL to next: only to the very same host (and port) and never from https to http.
func keepTokenOnRedirect(original, next *url.URL) bool {
	if !strings.EqualFold(original.Host, next.Host) {
		return false
	}
	return original.Scheme != schemeHTTPS || next.Scheme == schemeHTTPS
}

// verifiedCheckRedirect caps the redirects and drops the Authorization header unless every hop so far stayed on
// the original host without leaving https.
//
// net/http already strips Authorization when a redirect goes to a different domain, but it keeps it when the
// redirect goes to a subdomain of the original host and when it downgrades from https to http on the same host,
// which would send the token in clear text. This policy is stricter than that: it requires the exact same host
// and no downgrade, compared against the original request and against every hop in between, so that once the
// token is dropped it is not restored by a later redirect back to the original host.
func verifiedCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}

	original := via[0].URL
	keep := keepTokenOnRedirect(original, req.URL)
	for _, prev := range via[1:] {
		keep = keep && keepTokenOnRedirect(original, prev.URL)
	}
	if !keep {
		req.Header.Del("Authorization")
	}

	return nil
}

// newVerifiedClient returns the HTTP client used by DownloadVerified.
//
// Compression is disabled on the transport. Otherwise net/http asks for gzip and transparently decodes it, which
// would make the checksum cover the decoded bytes instead of the bytes on the wire and would leave the
// Content-Length of the response (the compressed size) out of sync with the number of bytes read, so truncation
// could not be detected reliably.
func newVerifiedClient() *http.Client {
	var tr *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	tr.DisableCompression = true

	return &http.Client{Transport: tr, CheckRedirect: verifiedCheckRedirect}
}

// DownloadVerified downloads fileURL to dst, retrying transient failures like FetchAndExtract does, and verifies the
// sha256 and maximum size of the content. On any failure no file is left at dst.
//
// Transient failures are retried with the same backoff as FetchAndExtract: connection errors, 502, 503 and 504
// responses and failures while reading the body (a reset connection or a body shorter than its Content-Length, for
// example). A checksum mismatch, a size violation and a failure writing dst are final.
//
// When opts.BearerToken is set, the URL must be https, or http to a loopback host, otherwise
// ErrInsecureTokenTransport is returned and nothing is sent.
func DownloadVerified(
	ctx context.Context, log logr.Logger, fileURL, dst string, opts VerifiedDownloadOptions,
) error {
	want := strings.ToLower(strings.TrimSpace(opts.SHA256))
	if b, err := hex.DecodeString(want); err != nil || len(b) != sha256.Size {
		return errors.New("expected sha256 must be a 64 character hex string")
	}

	if opts.BearerToken != "" {
		allowed, err := tokenTransportAllowed(fileURL)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrInsecureTokenTransport
		}
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	v := &verification{token: opts.BearerToken, sha256: want, max: opts.MaxSizeBytes}
	client := newVerifiedClient()

	err := wait.ExponentialBackoffWithContext(ctx, defaultBackoff, func(ctx context.Context) (bool, error) {
		return downloadFileWith(ctx, log, client, fileURL, dst, v)
	})
	if err != nil {
		_ = os.Remove(dst)
		return err
	}

	return nil
}
