package archive

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestDownloadVerified(t *testing.T) {
	fastBackoff(t, 4)
	body := []byte("hello verified world")
	good := sum(body)

	serve := func(t *testing.T, h http.HandlerFunc) (string, string) {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL, filepath.Join(t.TempDir(), "out")
	}
	ok := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }
	// stall blocks until the client gives up, with a safety net so a broken test cannot hang the suite.
	stall := func(r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}

	t.Run("success", func(t *testing.T) {
		u, dst := serve(t, ok)
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: strings.ToUpper(good), MaxSizeBytes: 1024}))
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, body, got)
	})

	t.Run("token sent", func(t *testing.T) {
		var hdr string
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			hdr = r.Header.Get("Authorization")
			ok(w, r)
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, BearerToken: "s3cret"}))
		require.Equal(t, "Bearer s3cret", hdr)
	})

	t.Run("no token when empty", func(t *testing.T) {
		var present bool
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, present = r.Header["Authorization"]
			ok(w, r)
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good}))
		require.False(t, present)
	})

	t.Run("token refused over plain http to a remote host", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "out")
		err := DownloadVerified(context.Background(), logr.Discard(), "http://example.com/asset", dst,
			VerifiedDownloadOptions{SHA256: good, BearerToken: "s3cret"})
		require.ErrorIs(t, err, ErrInsecureTokenTransport)
		require.NotContains(t, err.Error(), "s3cret")
		require.NoFileExists(t, dst)
	})

	t.Run("token refused for an unsupported scheme", func(t *testing.T) {
		err := DownloadVerified(context.Background(), logr.Discard(), "ftp://localhost/asset",
			filepath.Join(t.TempDir(), "out"), VerifiedDownloadOptions{SHA256: good, BearerToken: "s3cret"})
		require.ErrorIs(t, err, ErrInsecureTokenTransport)
	})

	t.Run("token not forwarded cross host", func(t *testing.T) {
		var hdr string
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hdr = r.Header.Get("Authorization")
			ok(w, r)
		}))
		defer other.Close()
		// 127.0.0.1 vs localhost differ as hosts
		target := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound)
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, BearerToken: "s3cret"}))
		require.Empty(t, hdr)
	})

	t.Run("sha mismatch removes file and is not retried", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			ok(w, r)
		})
		err := DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: sum([]byte("other"))})
		require.ErrorIs(t, err, ErrChecksumMismatch)
		require.NoFileExists(t, dst)
		require.EqualValues(t, 1, calls.Load())
	})

	t.Run("invalid sha", func(t *testing.T) {
		u, dst := serve(t, ok)
		require.Error(t, DownloadVerified(context.Background(), logr.Discard(), u, dst, VerifiedDownloadOptions{}))
	})

	t.Run("size cap via content length is not retried", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			ok(w, r)
		})
		err := DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, MaxSizeBytes: 5})
		require.ErrorIs(t, err, ErrSizeExceeded)
		require.NoFileExists(t, dst)
		require.EqualValues(t, 1, calls.Load())
	})

	t.Run("size cap via streaming is not retried", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			// chunked, no Content-Length
			chunk := make([]byte, 1024)
			for range 1000 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		})
		err := DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, MaxSizeBytes: 4096})
		require.ErrorIs(t, err, ErrSizeExceeded)
		require.NoFileExists(t, dst)
		require.EqualValues(t, 1, calls.Load())
	})

	t.Run("retry on 5xx then success", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			ok(w, r)
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good}))
		require.EqualValues(t, 3, calls.Load())
	})

	t.Run("retry when the body is cut mid transfer", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				// Promise the whole body and stop early, the server then closes the connection.
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body[:5])
				return
			}
			ok(w, r)
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good}))
		require.EqualValues(t, 2, calls.Load())
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, body, got)
	})

	t.Run("retry when the connection is reset mid transfer", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) > 1 {
				ok(w, r)
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
			_, _ = rw.Write(body[:5])
			_ = rw.Flush()
			time.Sleep(50 * time.Millisecond) // let the client start reading the body
			if tcp, isTCP := conn.(*net.TCPConn); isTCP {
				_ = tcp.SetLinger(0) // close with a RST instead of a FIN
			}
			_ = conn.Close()
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good}))
		require.EqualValues(t, 2, calls.Load())
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, body, got)
	})

	t.Run("gives up when the body is always cut", func(t *testing.T) {
		var calls atomic.Int32
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("partial"))
		})
		require.Error(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good}))
		require.NoFileExists(t, dst)
		require.EqualValues(t, 4, calls.Load()) // every step of fastBackoff(t, 4)
	})

	t.Run("cancelled context", func(t *testing.T) {
		u, dst := serve(t, ok)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := DownloadVerified(ctx, logr.Discard(), u, dst, VerifiedDownloadOptions{SHA256: good})
		require.Error(t, err)
		require.NoFileExists(t, dst)
	})

	t.Run("timeout while waiting for the response", func(t *testing.T) {
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) { stall(r) })
		start := time.Now()
		err := DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, Timeout: 100 * time.Millisecond})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), 5*time.Second)
		require.NoFileExists(t, dst)
	})

	t.Run("timeout while reading the body", func(t *testing.T) {
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body[:5])
			w.(http.Flusher).Flush()
			stall(r)
		})
		start := time.Now()
		err := DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, Timeout: 100 * time.Millisecond})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), 5*time.Second)
		require.NoFileExists(t, dst)
	})

	t.Run("timeout does not affect a fast download", func(t *testing.T) {
		u, dst := serve(t, ok)
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good, Timeout: time.Minute}))
		require.FileExists(t, dst)
	})

	t.Run("truncated body removes partial file", func(t *testing.T) {
		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("partial"))
		})
		require.Error(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: good}))
		require.NoFileExists(t, dst)
	})

	t.Run("compression is not negotiated", func(t *testing.T) {
		var acceptEncoding string
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, err := zw.Write(body)
		require.NoError(t, err)
		require.NoError(t, zw.Close())

		u, dst := serve(t, func(w http.ResponseWriter, r *http.Request) {
			acceptEncoding = r.Header.Get("Accept-Encoding")
			// A server that compresses anyway: the content is what was sent, not what it decodes to.
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz.Bytes())
		})
		require.NoError(t, DownloadVerified(context.Background(), logr.Discard(), u, dst,
			VerifiedDownloadOptions{SHA256: sum(gz.Bytes())}))
		require.Empty(t, acceptEncoding)
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, gz.Bytes(), got, "the file holds the wire bytes, not the decoded ones")
	})
}

func TestTokenTransportAllowed(t *testing.T) {
	for url, want := range map[string]bool{
		"https://example.com/a":              true,
		"https://example.com:8443/a":         true,
		"http://example.com/a":               false,
		"http://10.0.0.1/a":                  false,
		"http://localhost/a":                 true,
		"http://LOCALHOST:8080/a":            true,
		"http://127.0.0.1:8080/a":            true,
		"http://127.1.2.3/a":                 true,
		"http://[::1]:8080/a":                true,
		"http://localhost.example.com/a":     false,
		"http://127.0.0.1.example.com/a":     false,
		"http://[2001:db8::1]/a":             false,
		"ftp://localhost/a":                  false,
		"file:///etc/passwd":                 false,
		"//localhost/a":                      false,
		"localhost/a":                        false,
		"http://user:pw@example.com/a":       false,
		"http://localhost@example.com/a":     false,
		"https://localhost.example.com/a":    true,
		"HTTPS://EXAMPLE.COM/a":              true,
		"http://0.0.0.0/a":                   false,
		"http://[::ffff:127.0.0.1]:8080/a":   true,
		"http://[::ffff:10.0.0.1]:8080/a":    false,
		"http://127.0.0.1.nip.io:8080/a":     false,
		"http://localhost:80@example.com/a/": false,
	} {
		got, err := tokenTransportAllowed(url)
		require.NoError(t, err, url)
		require.Equal(t, want, got, url)
	}

	_, err := tokenTransportAllowed("http://bad host/")
	require.Error(t, err)
}

type seenRequest struct{ url, auth string }

// followRedirects downloads start through the client of DownloadVerified, with a transport that answers a redirect
// for every URL of routes and a 200 with body for any other URL. It returns what each request carried.
func followRedirects(t *testing.T, routes map[string]string, start string, body []byte) ([]seenRequest, error) {
	t.Helper()

	var seen []seenRequest
	c := newVerifiedClient()
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, seenRequest{url: r.URL.String(), auth: r.Header.Get("Authorization")})
		if next, ok := routes[r.URL.String()]; ok {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": {next}},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       r,
		}, nil
	})

	v := &verification{token: "s3cret", sha256: sum(body)}
	_, err := downloadFileWith(context.Background(), logr.Discard(), c, start, filepath.Join(t.TempDir(), "out"), v)
	return seen, err
}

func TestVerifiedRedirects(t *testing.T) {
	body := []byte("redirected body")
	bearer := "Bearer s3cret"

	tests := []struct {
		name   string
		start  string
		routes map[string]string
		auth   []string // Authorization expected by each request, in order
	}{
		{
			name:   "same host keeps the token",
			start:  "https://example.com/a",
			routes: map[string]string{"https://example.com/a": "https://example.com/b"},
			auth:   []string{bearer, bearer},
		},
		{
			name:   "other host drops the token",
			start:  "https://example.com/a",
			routes: map[string]string{"https://example.com/a": "https://other.org/b"},
			auth:   []string{bearer, ""},
		},
		{
			name:   "subdomain drops the token",
			start:  "https://example.com/a",
			routes: map[string]string{"https://example.com/a": "https://cdn.example.com/b"},
			auth:   []string{bearer, ""},
		},
		{
			name:   "other port drops the token",
			start:  "https://example.com/a",
			routes: map[string]string{"https://example.com/a": "https://example.com:8443/b"},
			auth:   []string{bearer, ""},
		},
		{
			name:   "https to http on the same host drops the token",
			start:  "https://example.com/a",
			routes: map[string]string{"https://example.com/a": "http://example.com/b"},
			auth:   []string{bearer, ""},
		},
		{
			name:  "token stays dropped when a later redirect returns to the original host",
			start: "https://example.com/a",
			routes: map[string]string{
				"https://example.com/a": "https://other.org/b",
				"https://other.org/b":   "https://example.com/c",
			},
			auth: []string{bearer, "", ""},
		},
		{
			name:  "token stays dropped after a downgrade and an upgrade back",
			start: "https://example.com/a",
			routes: map[string]string{
				"https://example.com/a": "http://example.com/b",
				"http://example.com/b":  "https://example.com/c",
			},
			auth: []string{bearer, "", ""},
		},
		{
			name:   "loopback http to the same host and port keeps the token",
			start:  "http://127.0.0.1:8080/a",
			routes: map[string]string{"http://127.0.0.1:8080/a": "http://127.0.0.1:8080/b"},
			auth:   []string{bearer, bearer},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen, err := followRedirects(t, tt.routes, tt.start, body)
			require.NoError(t, err)
			require.Len(t, seen, len(tt.auth))
			for i, want := range tt.auth {
				require.Equal(t, want, seen[i].auth, "request %d to %s", i, seen[i].url)
			}
		})
	}

	t.Run("stops after 10 redirects", func(t *testing.T) {
		routes := map[string]string{}
		for i := range 20 {
			routes["https://example.com/"+strconv.Itoa(i)] = "https://example.com/" + strconv.Itoa(i+1)
		}
		seen, err := followRedirects(t, routes, "https://example.com/0", body)
		require.ErrorContains(t, err, "stopped after 10 redirects")
		require.Len(t, seen, 10)
	})
}

// failingReader returns data and then err.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestCopyBodyErrorClasses(t *testing.T) {
	body := []byte("0123456789")
	v := &verification{sha256: sum(body), max: 1024}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

	t.Run("read failure is a read error", func(t *testing.T) {
		err := v.copyBody(io.Discard, &failingReader{data: body[:4], err: reset}, -1)
		var re *readError
		require.ErrorAs(t, err, &re)
		require.ErrorIs(t, err, syscall.ECONNRESET)
	})

	t.Run("unexpected EOF is a read error", func(t *testing.T) {
		err := v.copyBody(io.Discard, &failingReader{data: body[:4], err: io.ErrUnexpectedEOF}, -1)
		var re *readError
		require.ErrorAs(t, err, &re)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("body shorter than content length is a read error", func(t *testing.T) {
		err := v.copyBody(io.Discard, bytes.NewReader(body[:4]), int64(len(body)))
		var re *readError
		require.ErrorAs(t, err, &re)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("write failure is final", func(t *testing.T) {
		err := v.copyBody(failingWriter{err: syscall.ENOSPC}, bytes.NewReader(body), int64(len(body)))
		var re *readError
		require.NotErrorAs(t, err, &re)
		require.ErrorIs(t, err, syscall.ENOSPC)
	})

	t.Run("checksum mismatch is final", func(t *testing.T) {
		err := (&verification{sha256: sum([]byte("other"))}).copyBody(io.Discard, bytes.NewReader(body), -1)
		var re *readError
		require.NotErrorAs(t, err, &re)
		require.ErrorIs(t, err, ErrChecksumMismatch)
	})

	t.Run("size exceeded is final", func(t *testing.T) {
		err := (&verification{sha256: v.sha256, max: 4}).copyBody(io.Discard, bytes.NewReader(body), -1)
		var re *readError
		require.NotErrorAs(t, err, &re)
		require.ErrorIs(t, err, ErrSizeExceeded)
	})

	t.Run("a read failure removes the file and asks for a retry", func(t *testing.T) {
		c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{},
				ContentLength: -1,
				Body:          io.NopCloser(&failingReader{data: body[:4], err: reset}),
				Request:       r,
			}, nil
		})}
		fp := filepath.Join(t.TempDir(), "out")

		done, err := downloadFileWith(context.Background(), logr.Discard(), c, "https://example.com/a", fp, v)
		require.NoError(t, err)
		require.False(t, done)
		require.NoFileExists(t, fp)
	})
}
