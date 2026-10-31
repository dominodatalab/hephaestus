package buildkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	bkclient "github.com/moby/buildkit/client"
	"github.com/stretchr/testify/require"
)

type tarEntry struct {
	name string
	mode int64
	body string
	dir  bool
}

func makeArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.dir {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if !e.dir {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	return buf.Bytes()
}

func serve(t *testing.T, files map[string][]byte) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// bkTestClient needs a running buildkitd, e.g.
//
//	docker run -d --rm --privileged -p 127.0.0.1:11234:1234 moby/buildkit:v0.33.1 --addr tcp://0.0.0.0:1234
//	BUILDKIT_TEST_ADDR=tcp://127.0.0.1:11234 go test ./pkg/buildkit -run AdditionalContexts -v
func bkTestClient(t *testing.T) *Client {
	t.Helper()

	addr := os.Getenv("BUILDKIT_TEST_ADDR")
	if addr == "" {
		t.Skip("BUILDKIT_TEST_ADDR not set")
	}
	bk, err := bkclient.New(context.Background(), addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bk.Close() })

	return &Client{bk: bk, log: logr.Discard()}
}

func contextArchive(t *testing.T, dockerfile string) []byte {
	return makeArchive(t, []tarEntry{{name: "Dockerfile", mode: 0644, body: dockerfile}})
}

func assetsArchive(t *testing.T) []byte {
	return makeArchive(t, []tarEntry{
		{name: "app/", mode: 0755, dir: true},
		{name: "app/bin/", mode: 0755, dir: true},
		{name: "app/bin/tool", mode: 0755, body: "#!/bin/sh\necho tool\n"},
		{name: "app/conf/", mode: 0755, dir: true},
		{name: "app/conf/my file.txt", mode: 0644, body: "hello"},
		{name: "app/empty/", mode: 0755, dir: true},
	})
}

func TestBuildAdditionalContexts(t *testing.T) {
	c := bkTestClient(t)

	const dockerfile = `FROM busybox
COPY --from=assets app/bin/tool /usr/local/bin/tool
COPY --from=assets app/conf /opt/app/conf
COPY --from=assets app/empty /opt/app/empty
RUN test -x /usr/local/bin/tool \
 && grep -q hello "/opt/app/conf/my file.txt" \
 && test -d /opt/app/empty
RUN test ! -e /assets
`
	base := serve(t, map[string][]byte{
		"/context.tgz": contextArchive(t, dockerfile),
		"/assets.tgz":  assetsArchive(t),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	_, err := c.Build(ctx, BuildOptions{
		Context:            base + "/context.tgz",
		AdditionalContexts: map[string]string{"assets": base + "/assets.tgz"},
	})
	require.NoError(t, err)
}

// Sanity check: a build without an exporter must still run its steps, otherwise the test above proves nothing.
func TestBuildRunsStepsWithoutExporter(t *testing.T) {
	c := bkTestClient(t)

	base := serve(t, map[string][]byte{
		"/context.tgz": contextArchive(t, "FROM busybox\nRUN test -f /does-not-exist\n"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	_, err := c.Build(ctx, BuildOptions{Context: base + "/context.tgz"})
	require.Error(t, err)
}

func TestBuildWithoutAdditionalContextFails(t *testing.T) {
	c := bkTestClient(t)

	base := serve(t, map[string][]byte{
		"/context.tgz": contextArchive(t, "FROM busybox\nCOPY --from=assets app/bin/tool /tool\n"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	_, err := c.Build(ctx, BuildOptions{Context: base + "/context.tgz"})
	require.Error(t, err)
}

func TestAdditionalContextNameValidation(t *testing.T) {
	c := &Client{log: logr.Discard()}

	for _, name := range []string{"", "context", "dockerfile"} {
		_, err := c.fetchAdditionalContexts(context.Background(), t.TempDir(), BuildOptions{
			AdditionalContexts: map[string]string{name: "http://127.0.0.1:1/x.tgz"},
		})
		require.ErrorContains(t, err, "invalid additional context name")
	}
}
