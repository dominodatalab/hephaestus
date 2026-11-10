package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
)

type entry struct {
	hdr  tar.Header
	body string
}

// extractEntries writes a gzip tar of the given entries and extracts it into <wd>/extracted, as FetchAndExtract does.
// It returns the working directory, everything logged during extraction, and the extraction error.
func extractEntries(t *testing.T, entries []entry) (wd string, logs string, err error) {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		require.NoError(t, tw.WriteHeader(&h))
		_, werr := tw.Write([]byte(e.body))
		require.NoError(t, werr)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	wd = t.TempDir()
	fp := filepath.Join(wd, "archive")
	require.NoError(t, os.WriteFile(fp, buf.Bytes(), 0o600))
	dst := filepath.Join(wd, "extracted")
	require.NoError(t, os.Mkdir(dst, 0o755))

	var sb strings.Builder
	var log logr.Logger = funcr.New(func(_, args string) { sb.WriteString(args + "\n") }, funcr.Options{})

	err = extract(log, fp, mimeTypeGzip, dst)

	return wd, sb.String(), err
}

func TestExtractRejectsParentTraversal(t *testing.T) {
	wd, _, err := extractEntries(t, []entry{{hdr: tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}})
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(wd, "outside"))
}

// A destination of <wd>/extracted must not allow writes to a sibling such as <wd>/extracted-evil.
func TestExtractRejectsSiblingPrefixPath(t *testing.T) {
	wd, _, err := extractEntries(t, []entry{
		{hdr: tar.Header{Name: "../extracted-evil/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "../extracted-evil/x", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
	})
	require.Error(t, err)
	require.NoDirExists(t, filepath.Join(wd, "extracted-evil"))
}

// Archives made with "tar -C dir ." start with a "./" entry that resolves to the destination itself.
func TestExtractAllowsCurrentDirectoryEntries(t *testing.T) {
	wd, _, err := extractEntries(t, []entry{
		{hdr: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "./a.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "hello"},
	})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(wd, "extracted", "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(got))
}

// Links are not extracted, but dropping them without a trace would change a build without telling anyone.
func TestExtractSkipsLinksAndLogsThem(t *testing.T) {
	wd, logs, err := extractEntries(t, []entry{
		{hdr: tar.Header{Name: "target", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
		{hdr: tar.Header{Name: "sym", Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0o777}},
		{hdr: tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "target", Mode: 0o644}},
	})
	require.NoError(t, err)

	require.FileExists(t, filepath.Join(wd, "extracted", "target"))
	for _, name := range []string{"sym", "hard"} {
		_, lerr := os.Lstat(filepath.Join(wd, "extracted", name))
		require.True(t, os.IsNotExist(lerr), "%s should not be extracted", name)
		require.Contains(t, logs, `"name"="`+name+`"`)
	}
	require.Contains(t, logs, "Skipping link entry")
}

// Special files are never created: a device node or FIFO in an archive must not reach the filesystem.
func TestExtractIgnoresSpecialFiles(t *testing.T) {
	wd, _, err := extractEntries(t, []entry{
		{hdr: tar.Header{Name: "keep", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
		{hdr: tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o644}},
		{hdr: tar.Header{Name: "chr", Typeflag: tar.TypeChar, Mode: 0o644, Devmajor: 1, Devminor: 3}},
		{hdr: tar.Header{Name: "blk", Typeflag: tar.TypeBlock, Mode: 0o644, Devmajor: 7, Devminor: 0}},
	})
	require.NoError(t, err)

	require.FileExists(t, filepath.Join(wd, "extracted", "keep"))
	for _, name := range []string{"fifo", "chr", "blk"} {
		_, lerr := os.Lstat(filepath.Join(wd, "extracted", name))
		require.True(t, os.IsNotExist(lerr), "%s should not be extracted", name)
	}
}

// Archives need not contain an entry for every parent directory.
func TestExtractFileWithoutDirectoryEntry(t *testing.T) {
	wd, _, err := extractEntries(t, []entry{{hdr: tar.Header{Name: "a/b.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(wd, "extracted", "a", "b.txt"))
}

// Creating the parent directory of a file must fail, not overwrite or skip, when the parent is itself a file.
func TestExtractFailsWhenParentIsAFile(t *testing.T) {
	_, _, err := extractEntries(t, []entry{
		{hdr: tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
		{hdr: tar.Header{Name: "a/b.txt", Typeflag: tar.TypeReg, Mode: 0o644}, body: "y"},
	})
	require.Error(t, err)
}
