package archive

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestFetchAndExtract(t *testing.T) {
	ctx := context.Background()
	log := logr.Discard()

	t.Run("working_dir_err", func(t *testing.T) {
		_, err := FetchAndExtract(ctx, log, "fake-url", "/does/not/exist", 0)
		require.EqualError(t, err, "invalid docker working dir: stat /does/not/exist: no such file or directory")
	})

	t.Run("timeout_err", func(t *testing.T) {
		_, err := FetchAndExtract(ctx, log, "fake-url", t.TempDir(), 1*time.Nanosecond)
		require.EqualError(t, err, "failed to download remote docker context: context deadline exceeded")
	})

	t.Run("http_errors", func(t *testing.T) {
		origCap := defaultBackoff.Cap
		defaultBackoff = wait.Backoff{
			Duration: time.Nanosecond,
			Factor:   2,
			Jitter:   0.1,
			Steps:    4,
		}

		t.Cleanup(func() {
			defaultBackoff.Cap = origCap
		})

		t.Run("conn_refused", func(t *testing.T) {
			defaultHTTPClient = &errClient{
				err: &net.OpError{
					Op:   "dial",
					Net:  "tcp",
					Addr: nil,
					Err: &os.SyscallError{
						Syscall: "connect",
						Err:     syscall.ECONNREFUSED,
					},
				},
			}
			t.Cleanup(func() { defaultHTTPClient = http.DefaultClient })

			_, err := FetchAndExtract(ctx, log, "test-url", t.TempDir(), 0)
			require.Error(t, err)
		})

		t.Run("transient", func(t *testing.T) {
			idx := 0
			transientStatuses := []int{http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusServiceUnavailable}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(transientStatuses[idx%len(transientStatuses)])
				idx++
			}))
			defer srv.Close()

			defaultHTTPClient = srv.Client()
			t.Cleanup(func() { defaultHTTPClient = http.DefaultClient })

			_, err := FetchAndExtract(ctx, log, srv.URL, t.TempDir(), 0)
			require.EqualError(t, err, "failed to download remote docker context: maximum number of retries exceeded")
		})

		t.Run("critical", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()

			defaultHTTPClient = srv.Client()
			t.Cleanup(func() { defaultHTTPClient = http.DefaultClient })

			_, err := FetchAndExtract(ctx, log, srv.URL, t.TempDir(), 0)
			require.EqualError(t, err, "failed to download remote docker context: file download failed with status 403")
		})
	})

	t.Run("file_types", func(t *testing.T) {
		testcases := []struct {
			name    string
			archive string
			errMsg  string
		}{
			{
				name:    "tarball",
				archive: "testdata/simple-app.tar",
			},
			{
				name:    "gzipped_tarball",
				archive: "testdata/simple-app.tgz",
			},
			{
				name:    "zipfile",
				archive: "testdata/simple-app.zip",
				errMsg:  `unsupported file content type "application/zip"`,
			},
			{
				name:    "garbage",
				archive: "testdata/this-is-not-a.tar",
				errMsg:  "cannot sniff content type for file with 0 bytes",
			},
		}

		srv := httptest.NewServer(nil)
		defer srv.Close()

		for _, tc := range testcases {
			t.Run(tc.name, func(t *testing.T) {
				srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					bs, err := os.ReadFile(tc.archive)
					require.NoError(t, err)

					_, err = w.Write(bs)
					require.NoError(t, err)
				})

				ext, err := FetchAndExtract(ctx, log, srv.URL, t.TempDir(), 0)
				if tc.errMsg != "" {
					require.EqualError(t, err, tc.errMsg)
					return
				}

				require.NoError(t, err)
				assert.FileExists(t, ext.Archive)

				var actual []string
				err = filepath.Walk(ext.ContentsDir, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}

					if info.IsDir() {
						return nil
					}

					path, _ = filepath.Rel(ext.ContentsDir, path)
					actual = append(actual, path)

					return nil
				})
				assert.ElementsMatch(t, []string{"Dockerfile", "app.py"}, actual)
			})
		}
	})
}

type errClient struct {
	err error
}

func (c *errClient) Get(string) (*http.Response, error) {
	return nil, &url.Error{Err: c.err}
}
