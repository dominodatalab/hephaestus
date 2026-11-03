package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
)

// fastBackoff replaces the retry schedule, which otherwise waits seconds between attempts.
func fastBackoff(t *testing.T, steps int) {
	t.Helper()

	orig := defaultBackoff
	defaultBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: steps}
	t.Cleanup(func() { defaultBackoff = orig })
}

// tarBytes returns a tar, optionally gzipped, holding one file.
func tarBytes(t *testing.T, name, body string, gzipped bool) []byte {
	t.Helper()

	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}))
	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	if !gzipped {
		return tb.Bytes()
	}

	var gb bytes.Buffer
	gz := gzip.NewWriter(&gb)
	_, err = gz.Write(tb.Bytes())
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	return gb.Bytes()
}

func fetch(t *testing.T, srvURL string, timeout time.Duration) (*Extraction, error) {
	t.Helper()

	return FetchAndExtract(context.Background(), logr.Discard(), srvURL, t.TempDir(), timeout)
}

func TestFetchAndExtractOverHTTP(t *testing.T) {
	for _, gzipped := range []bool{true, false} {
		t.Run(fmt.Sprintf("gzip=%v", gzipped), func(t *testing.T) {
			archive := tarBytes(t, "a.txt", "hello", gzipped)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
			defer srv.Close()

			ex, err := fetch(t, srv.URL, 0)
			require.NoError(t, err)

			got, err := os.ReadFile(filepath.Join(ex.ContentsDir, "a.txt"))
			require.NoError(t, err)
			require.Equal(t, "hello", string(got))
		})
	}
}

// Only 502, 503 and 504 are worth retrying: other failures must surface after a single request.
func TestFetchAndExtractDoesNotRetryOtherStatuses(t *testing.T) {
	fastBackoff(t, 5)

	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(code)
			}))
			defer srv.Close()

			_, err := fetch(t, srv.URL, 0)
			require.ErrorContains(t, err, fmt.Sprintf("file download failed with status %d", code))
			require.EqualValues(t, 1, hits.Load())
		})
	}
}

func TestFetchAndExtractRetriesTransientStatuses(t *testing.T) {
	fastBackoff(t, 5)
	archive := tarBytes(t, "a.txt", "hello", true)

	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) <= 2 {
					w.WriteHeader(code)
					return
				}
				_, _ = w.Write(archive)
			}))
			defer srv.Close()

			ex, err := fetch(t, srv.URL, 0)
			require.NoError(t, err)
			require.FileExists(t, filepath.Join(ex.ContentsDir, "a.txt"))
			require.EqualValues(t, 3, hits.Load())
		})
	}
}

func TestFetchAndExtractGivesUpOnPersistentTransientStatus(t *testing.T) {
	fastBackoff(t, 3)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := fetch(t, srv.URL, 0)
	require.Error(t, err)
	require.EqualValues(t, 3, hits.Load(), "one attempt per backoff step")
}

// A refused connection is an operational error before any HTTP response, so it is retried until the backoff runs out.
func TestFetchAndExtractRetriesConnectionRefused(t *testing.T) {
	fastBackoff(t, 3)

	srv := httptest.NewServer(http.NotFoundHandler())
	closedURL := srv.URL
	srv.Close()

	start := time.Now()
	_, err := fetch(t, closedURL, 0)
	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"operational error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"timeout", context.DeadlineExceeded, true},
		{"anything else", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, retryable(&url.Error{Op: "Get", URL: "http://x", Err: tt.err}))
		})
	}
}

func TestFetchAndExtractRejectsBadContent(t *testing.T) {
	corruptGzip := append([]byte{0x1f, 0x8b, 0x08, 0x00}, bytes.Repeat([]byte{0xff}, 600)...)

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"unsupported content type", []byte("hello world"), "unsupported file content type"},
		{"empty body", nil, "EOF"},
		{"corrupt gzip", corruptGzip, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tt.body) }))
			defer srv.Close()

			_, err := fetch(t, srv.URL, 0)
			require.Error(t, err)
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

// The timeout bounds the download and its retries together.
func TestFetchAndExtractTimeout(t *testing.T) {
	fastBackoff(t, 3)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()

	start := time.Now()
	_, err := fetch(t, srv.URL, 100*time.Millisecond)
	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestFetchAndExtractInvalidWorkingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := FetchAndExtract(context.Background(), logr.Discard(), "http://127.0.0.1:1/x", missing, 0)
	require.ErrorContains(t, err, "invalid build context directory")

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = FetchAndExtract(context.Background(), logr.Discard(), "http://127.0.0.1:1/x", file, 0)
	require.ErrorContains(t, err, "is not a directory")
}
