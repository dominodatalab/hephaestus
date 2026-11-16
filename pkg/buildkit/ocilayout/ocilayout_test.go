package ocilayout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var linuxAMD64 = Options{OS: "linux", Architecture: "amd64"}

type entry struct{ name, body string }

// makeTar builds a tar archive with the given header format (tar.FormatUnknown lets the writer pick).
func makeTar(t *testing.T, format tar.Format, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Format: format,
		}))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())

	return buf.Bytes()
}

func simpleTar(t *testing.T) []byte {
	t.Helper()

	return makeTar(t, tar.FormatUnknown, entry{"a.txt", "hello"}, entry{"b/c.txt", "world"})
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

// padded returns magic followed by zero padding up to n bytes.
func padded(magic []byte, n int) []byte {
	return append(append([]byte{}, magic...), make([]byte, n-len(magic))...)
}

// writeSource writes data to a fresh file with mode 0600 and returns its path.
func writeSource(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.tar")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

// writer is a way of producing a layout from tar bytes: from a stream (Write) or from a file (WriteFile).
type writer struct {
	name string
	fn   func(t *testing.T, data []byte, dest string, opts Options) (digest.Digest, error)
}

func writers() []writer {
	return []writer{
		{"Write", func(_ *testing.T, data []byte, dest string, opts Options) (digest.Digest, error) {
			return Write(bytes.NewReader(data), dest, opts)
		}},
		{"WriteFile", func(t *testing.T, data []byte, dest string, opts Options) (digest.Digest, error) {
			return WriteFile(writeSource(t, data), dest, opts)
		}},
	}
}

// eachWriter runs fn as a subtest for both Write and WriteFile with a fresh parent and destination.
func eachWriter(t *testing.T, fn func(t *testing.T, w writer, parent, dest string)) {
	t.Helper()
	for _, w := range writers() {
		t.Run(w.name, func(t *testing.T) {
			parent := t.TempDir()
			fn(t, w, parent, filepath.Join(parent, "layout"))
		})
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, v))
}

// verifyBlob checks that the blob for desc exists with matching digest and size, and returns it.
func verifyBlob(t *testing.T, dir string, desc ocispec.Descriptor) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", desc.Digest.Encoded()))
	require.NoError(t, err)
	assert.Equal(t, desc.Size, int64(len(data)), "size mismatch")
	assert.Equal(t, desc.Digest, digest.FromBytes(data), "digest mismatch")

	return data
}

// snapshot maps every path under dir to its mode and content digest, for comparing layouts.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel] = fmt.Sprintf("dir %v", info.Mode().Perm())

			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("file %v %s", info.Mode().Perm(), digest.FromBytes(data))

		return nil
	})
	require.NoError(t, err)

	return out
}

// requireNoEntries asserts that dir is empty, i.e. no layout and no staging directory was left behind.
func requireNoEntries(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestWrite_ValidLayout(t *testing.T) {
	tests := []struct {
		name string
		tar  []byte
		opts Options
		want ocispec.Platform
	}{
		{
			name: "linux amd64",
			tar:  simpleTar(t),
			opts: linuxAMD64,
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name: "linux arm64 with variant",
			tar:  makeTar(t, tar.FormatUnknown, entry{"a", "x"}),
			opts: Options{OS: "linux", Architecture: "arm64", Variant: "v8"},
			want: ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		},
		{
			name: "empty tar archive",
			tar:  makeTar(t, tar.FormatUnknown),
			opts: linuxAMD64,
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name: "ustar header",
			tar:  makeTar(t, tar.FormatUSTAR, entry{"a", "x"}),
			opts: linuxAMD64,
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name: "gnu header",
			tar:  makeTar(t, tar.FormatGNU, entry{"a", "x"}),
			opts: linuxAMD64,
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name: "pax header with long name",
			tar:  makeTar(t, tar.FormatPAX, entry{strings.Repeat("n", 150), "x"}),
			opts: linuxAMD64,
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name: "single zero block",
			tar:  make([]byte, 512),
			opts: linuxAMD64,
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachWriter(t, func(t *testing.T, w writer, _, dest string) {
				mdgst, err := w.fn(t, tt.tar, dest, tt.opts)
				require.NoError(t, err)

				info, err := os.Stat(dest)
				require.NoError(t, err)
				assert.Equal(t, fs.FileMode(0o755), info.Mode().Perm(), "layout dir mode")

				var layout ocispec.ImageLayout
				readJSON(t, filepath.Join(dest, "oci-layout"), &layout)
				assert.Equal(t, ocispec.ImageLayoutVersion, layout.Version)

				var index ocispec.Index
				readJSON(t, filepath.Join(dest, "index.json"), &index)
				require.Equal(t, 2, index.SchemaVersion)
				require.Len(t, index.Manifests, 1)
				md := index.Manifests[0]
				assert.Equal(t, mdgst, md.Digest)
				assert.Equal(t, ocispec.MediaTypeImageManifest, md.MediaType)
				require.NotNil(t, md.Platform)
				assert.Equal(t, tt.want, *md.Platform)

				var manifest ocispec.Manifest
				require.NoError(t, json.Unmarshal(verifyBlob(t, dest, md), &manifest))
				assert.Equal(t, ocispec.MediaTypeImageConfig, manifest.Config.MediaType)
				require.Len(t, manifest.Layers, 1)
				assert.Equal(t, ocispec.MediaTypeImageLayer, manifest.Layers[0].MediaType)

				layer := verifyBlob(t, dest, manifest.Layers[0])
				assert.True(t, bytes.Equal(layer, tt.tar), "layer blob differs from input tar")

				var cfg ocispec.Image
				require.NoError(t, json.Unmarshal(verifyBlob(t, dest, manifest.Config), &cfg))
				assert.Equal(t, tt.want, cfg.Platform)
				assert.Equal(t, "layers", cfg.RootFS.Type)
				assert.Equal(t, []digest.Digest{digest.FromBytes(tt.tar)}, cfg.RootFS.DiffIDs)

				blobs := filepath.Join(dest, "blobs", "sha256")
				entries, err := os.ReadDir(blobs)
				require.NoError(t, err)
				assert.Len(t, entries, 3, "expected layer, config and manifest blobs")
				for _, e := range entries {
					fi, err := e.Info()
					require.NoError(t, err)
					assert.Equal(t, fs.FileMode(0o644), fi.Mode().Perm(), e.Name())
				}

				// Nothing but the layout is left next to it.
				siblings, err := os.ReadDir(filepath.Dir(dest))
				require.NoError(t, err)
				assert.Len(t, siblings, 1)
			})
		})
	}
}

func TestWrite_PlatformRequired(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "zero options", opts: Options{}},
		{name: "missing architecture", opts: Options{OS: "linux"}},
		{name: "missing os", opts: Options{Architecture: "amd64"}},
		{name: "variant alone", opts: Options{Variant: "v8"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachWriter(t, func(t *testing.T, w writer, parent, dest string) {
				_, err := w.fn(t, simpleTar(t), dest, tt.opts)
				require.ErrorIs(t, err, ErrPlatformRequired)
				requireNoEntries(t, parent)
			})
		})
	}
}

func TestWrite_InputValidation(t *testing.T) {
	valid := simpleTar(t)

	badChecksum := bytes.Clone(valid)
	badChecksum[0] ^= 0x01 // the name changes, the stored checksum does not

	noMagic := bytes.Clone(valid)
	clear(noMagic[257:265]) // a V7-style header

	badVersion := bytes.Clone(valid)
	badVersion[262] = 'x'

	garbageChecksum := bytes.Clone(valid)
	copy(garbageChecksum[148:156], "zzzzzzzz")

	tests := []struct {
		name    string
		data    []byte
		wantErr error
		wantMsg string
	}{
		{name: "valid tar", data: valid},
		{name: "empty tar archive", data: makeTar(t, tar.FormatUnknown)},
		{name: "single zero block", data: make([]byte, 512)},

		{name: "zero bytes", data: nil, wantErr: ErrEmpty},

		{name: "gzip", data: gzipBytes(t, valid), wantErr: ErrCompressed, wantMsg: "gzip"},
		{name: "gzip magic only", data: []byte{0x1f, 0x8b, 0x08}, wantErr: ErrCompressed, wantMsg: "gzip"},
		{name: "zstd", data: padded([]byte{0x28, 0xb5, 0x2f, 0xfd}, 700), wantErr: ErrCompressed, wantMsg: "zstd"},
		{name: "xz", data: padded([]byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, 700), wantErr: ErrCompressed, wantMsg: "xz"},

		{name: "text", data: []byte(strings.Repeat("hello world\n", 100)), wantErr: ErrNotTar},
		{name: "short input", data: []byte(strings.Repeat("x", 11)), wantErr: ErrNotTar},
		{name: "short tar prefix", data: valid[:511], wantErr: ErrNotTar},
		{name: "wrong checksum", data: badChecksum, wantErr: ErrNotTar},
		{name: "unparsable checksum", data: garbageChecksum, wantErr: ErrNotTar},
		{name: "missing magic", data: noMagic, wantErr: ErrNotTar},
		{name: "wrong version separator", data: badVersion, wantErr: ErrNotTar},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachWriter(t, func(t *testing.T, w writer, parent, dest string) {
				_, err := w.fn(t, tt.data, dest, linuxAMD64)
				if tt.wantErr == nil {
					require.NoError(t, err)

					return
				}
				require.ErrorIs(t, err, tt.wantErr)
				assert.Contains(t, err.Error(), tt.wantMsg)
				requireNoEntries(t, parent)
			})
		})
	}
}

func TestWrite_ValidationDoesNotBufferInput(t *testing.T) {
	// Validation peeks at the head of the stream; check that the peeked bytes are not lost or
	// duplicated for input far larger than one block, delivered in uneven reads.
	big := makeTar(t, tar.FormatUnknown, entry{"big", strings.Repeat("0123456789abcdef", 64<<10)})
	require.Greater(t, len(big), 2*copyBufSize)

	dest := filepath.Join(t.TempDir(), "layout")
	mdgst, err := Write(io.MultiReader(bytes.NewReader(big[:700]), bytes.NewReader(big[700:])), dest, linuxAMD64)
	require.NoError(t, err)

	var index ocispec.Index
	readJSON(t, filepath.Join(dest, "index.json"), &index)
	assert.Equal(t, mdgst, index.Manifests[0].Digest)
	blob, err := os.ReadFile(filepath.Join(dest, "blobs", "sha256", digest.FromBytes(big).Encoded()))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(big, blob))
}

func TestWrite_Deterministic(t *testing.T) {
	data := makeTar(t, tar.FormatUnknown, entry{"a", "1"}, entry{"b", "2"})

	var digests []digest.Digest
	var indexes [][]byte
	for range 2 {
		dest := filepath.Join(t.TempDir(), "layout")
		d, err := Write(bytes.NewReader(data), dest, linuxAMD64)
		require.NoError(t, err)
		digests = append(digests, d)
		idx, err := os.ReadFile(filepath.Join(dest, "index.json"))
		require.NoError(t, err)
		indexes = append(indexes, idx)
	}
	assert.Equal(t, digests[0], digests[1])
	assert.Equal(t, indexes[0], indexes[1])

	other := makeTar(t, tar.FormatUnknown, entry{"a", "1"}, entry{"b", "3"})
	d, err := Write(bytes.NewReader(other), filepath.Join(t.TempDir(), "layout"), linuxAMD64)
	require.NoError(t, err)
	assert.NotEqual(t, digests[0], d, "different tars produced the same manifest digest")

	// The platform is part of the config and so of the manifest digest.
	arm := Options{OS: "linux", Architecture: "arm64"}
	d, err = Write(bytes.NewReader(data), filepath.Join(t.TempDir(), "layout"), arm)
	require.NoError(t, err)
	assert.NotEqual(t, digests[0], d, "different platforms produced the same manifest digest")
}

func TestWrite_Destination(t *testing.T) {
	data := simpleTar(t)

	t.Run("missing with missing parents", func(t *testing.T) {
		eachWriter(t, func(t *testing.T, w writer, parent, _ string) {
			dest := filepath.Join(parent, "a", "b", "layout")
			_, err := w.fn(t, data, dest, linuxAMD64)
			require.NoError(t, err)
			assert.FileExists(t, filepath.Join(dest, "index.json"))
		})
	})

	t.Run("empty directory is replaced", func(t *testing.T) {
		eachWriter(t, func(t *testing.T, w writer, parent, dest string) {
			require.NoError(t, os.Mkdir(dest, 0o700))
			_, err := w.fn(t, data, dest, linuxAMD64)
			require.NoError(t, err)
			assert.FileExists(t, filepath.Join(dest, "index.json"))
			info, err := os.Stat(dest)
			require.NoError(t, err)
			assert.Equal(t, fs.FileMode(0o755), info.Mode().Perm())
			entries, err := os.ReadDir(parent)
			require.NoError(t, err)
			assert.Len(t, entries, 1, "staging directory left behind")
		})
	})

	t.Run("non-empty directory is rejected", func(t *testing.T) {
		eachWriter(t, func(t *testing.T, w writer, parent, dest string) {
			require.NoError(t, os.Mkdir(dest, 0o755))
			keep := filepath.Join(dest, "keep")
			require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o600))

			_, err := w.fn(t, data, dest, linuxAMD64)
			require.ErrorIs(t, err, ErrDestinationNotEmpty)

			got, err := os.ReadFile(keep)
			require.NoError(t, err)
			assert.Equal(t, "mine", string(got))
			assert.NoFileExists(t, filepath.Join(dest, "index.json"))
			entries, err := os.ReadDir(parent)
			require.NoError(t, err)
			assert.Len(t, entries, 1, "staging directory left behind")
		})
	})

	t.Run("regular file is rejected", func(t *testing.T) {
		eachWriter(t, func(t *testing.T, w writer, _, dest string) {
			require.NoError(t, os.WriteFile(dest, []byte("mine"), 0o600))

			_, err := w.fn(t, data, dest, linuxAMD64)
			require.ErrorIs(t, err, ErrDestinationNotEmpty)

			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			assert.Equal(t, "mine", string(got))
		})
	})

	t.Run("existing layout is not overwritten", func(t *testing.T) {
		eachWriter(t, func(t *testing.T, w writer, _, dest string) {
			d1, err := w.fn(t, data, dest, linuxAMD64)
			require.NoError(t, err)
			before := snapshot(t, dest)

			_, err = w.fn(t, data, dest, linuxAMD64)
			require.ErrorIs(t, err, ErrDestinationNotEmpty)
			assert.Equal(t, before, snapshot(t, dest), "first layout was modified")

			// Rewriting is done by removing the layout first, and is deterministic.
			require.NoError(t, os.RemoveAll(dest))
			d2, err := w.fn(t, data, dest, linuxAMD64)
			require.NoError(t, err)
			assert.Equal(t, d1, d2)
			assert.Equal(t, before, snapshot(t, dest))
		})
	})

	t.Run("destination checked before input is consumed", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "layout")
		require.NoError(t, os.Mkdir(dest, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dest, "keep"), nil, 0o600))

		_, err := Write(errReader{errors.New("must not be read")}, dest, linuxAMD64)
		require.ErrorIs(t, err, ErrDestinationNotEmpty)
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// failAfter yields data and then fails instead of returning io.EOF.
type failAfter struct {
	data []byte
	err  error
}

func (f *failAfter) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]

	return n, nil
}

func TestWrite_Errors(t *testing.T) {
	boom := errors.New("boom")
	valid := simpleTar(t)

	tests := []struct {
		name    string
		r       io.Reader
		opts    Options
		wantErr error
	}{
		{name: "zero bytes", r: bytes.NewReader(nil), opts: linuxAMD64, wantErr: ErrEmpty},
		{
			name:    "over limit",
			r:       bytes.NewReader(valid),
			opts:    Options{OS: "linux", Architecture: "amd64", MaxBytes: int64(len(valid)) - 1},
			wantErr: ErrTooLarge,
		},
		{name: "reader failure at start", r: errReader{boom}, opts: linuxAMD64, wantErr: boom},
		{
			name:    "reader failure mid-stream",
			r:       &failAfter{data: valid[:600], err: boom},
			opts:    linuxAMD64,
			wantErr: boom,
		},
		{name: "not a tar", r: strings.NewReader(strings.Repeat("x", 600)), opts: linuxAMD64, wantErr: ErrNotTar},
		{name: "compressed", r: bytes.NewReader(gzipBytes(t, valid)), opts: linuxAMD64, wantErr: ErrCompressed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "layout")

			_, err := Write(tt.r, dest, tt.opts)
			require.ErrorIs(t, err, tt.wantErr)
			// Neither a layout nor a staging directory may remain.
			requireNoEntries(t, parent)
		})
	}

	t.Run("unwritable parent", func(t *testing.T) {
		fileAsDir := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(fileAsDir, nil, 0o600))

		_, err := Write(bytes.NewReader(valid), filepath.Join(fileAsDir, "sub", "layout"), linuxAMD64)
		require.Error(t, err)
	})
}

func TestWrite_ExactLimit(t *testing.T) {
	data := simpleTar(t)
	opts := Options{OS: "linux", Architecture: "amd64", MaxBytes: int64(len(data))}
	eachWriter(t, func(t *testing.T, w writer, _, dest string) {
		_, err := w.fn(t, data, dest, opts)
		require.NoError(t, err, "input equal to limit should pass")
	})
}

func TestWriteFile(t *testing.T) {
	data := simpleTar(t)
	path := writeSource(t, data)

	fromFile, err := WriteFile(path, filepath.Join(t.TempDir(), "layout"), linuxAMD64)
	require.NoError(t, err)
	fromReader, err := Write(bytes.NewReader(data), filepath.Join(t.TempDir(), "layout"), linuxAMD64)
	require.NoError(t, err)
	assert.Equal(t, fromReader, fromFile)

	t.Run("missing file", func(t *testing.T) {
		parent := t.TempDir()
		_, err := WriteFile(filepath.Join(t.TempDir(), "missing.tar"), filepath.Join(parent, "layout"), linuxAMD64)
		require.ErrorIs(t, err, fs.ErrNotExist)
		requireNoEntries(t, parent)
	})

	t.Run("not a regular file", func(t *testing.T) {
		parent := t.TempDir()
		_, err := WriteFile(t.TempDir(), filepath.Join(parent, "layout"), linuxAMD64)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
		requireNoEntries(t, parent)
	})

	t.Run("empty file", func(t *testing.T) {
		parent := t.TempDir()
		_, err := WriteFile(writeSource(t, nil), filepath.Join(parent, "layout"), linuxAMD64)
		require.ErrorIs(t, err, ErrEmpty)
		requireNoEntries(t, parent)
	})

	t.Run("over limit", func(t *testing.T) {
		parent := t.TempDir()
		opts := Options{OS: "linux", Architecture: "amd64", MaxBytes: int64(len(data)) - 1}
		_, err := WriteFile(path, filepath.Join(parent, "layout"), opts)
		require.ErrorIs(t, err, ErrTooLarge)
		requireNoEntries(t, parent)
	})
}

// failPlacement makes the given placement primitive fail the way a cross-device operation does.
func failPlacement(t *testing.T, target *func(string, string) error) {
	t.Helper()
	orig := *target
	*target = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "place", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { *target = orig })
}

func TestWriteFile_Placement(t *testing.T) {
	data := simpleTar(t)

	// The reference layout is produced by streaming through Write.
	refDest := filepath.Join(t.TempDir(), "layout")
	refDigest, err := Write(bytes.NewReader(data), refDest, linuxAMD64)
	require.NoError(t, err)
	ref := snapshot(t, refDest)
	layerBlob := digest.FromBytes(data).Encoded()

	tests := []struct {
		name         string
		move         bool
		breakPrimary func(*testing.T)
		// wantShared is true when the blob must share its inode with the source.
		wantShared bool
		// wantSourceGone is true when the source must have been consumed.
		wantSourceGone bool
	}{
		{name: "hard link", wantShared: true},
		{name: "rename", move: true, wantSourceGone: true},
		{
			name:         "link falls back to copy",
			breakPrimary: func(t *testing.T) { failPlacement(t, &linkFile) },
		},
		{
			name:           "rename falls back to copy and removes the source",
			move:           true,
			breakPrimary:   func(t *testing.T) { failPlacement(t, &renameFile) },
			wantSourceGone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.breakPrimary != nil {
				tt.breakPrimary(t)
			}
			src := writeSource(t, data) // mode 0600

			dest := filepath.Join(t.TempDir(), "layout")
			opts := linuxAMD64
			opts.MoveFile = tt.move
			got, err := WriteFile(src, dest, opts)
			require.NoError(t, err)

			// Identical digests, content and permissions whichever way the blob got there.
			assert.Equal(t, refDigest, got)
			assert.Equal(t, ref, snapshot(t, dest))

			blob := filepath.Join(dest, "blobs", "sha256", layerBlob)
			blobInfo, err := os.Stat(blob)
			require.NoError(t, err)
			assert.Equal(t, fs.FileMode(0o644), blobInfo.Mode().Perm())

			if tt.wantSourceGone {
				assert.NoFileExists(t, src)

				return
			}
			after, err := os.Stat(src)
			require.NoError(t, err, "source must survive without MoveFile")
			assert.Equal(t, tt.wantShared, os.SameFile(after, blobInfo), "hard link vs copy")
			onDisk, err := os.ReadFile(src)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(data, onDisk), "source content changed")
		})
	}

	t.Run("both link and copy fallback fail", func(t *testing.T) {
		// The source vanishes right as it is placed, so the link fails and so does the copy.
		orig := linkFile
		linkFile = func(oldpath, newpath string) error {
			_ = os.Remove(oldpath)

			return &os.LinkError{Op: "link", Old: oldpath, New: newpath, Err: syscall.EXDEV}
		}
		t.Cleanup(func() { linkFile = orig })

		parent := t.TempDir()
		_, err := WriteFile(writeSource(t, data), filepath.Join(parent, "layout"), linuxAMD64)
		require.ErrorIs(t, err, fs.ErrNotExist)
		assert.Contains(t, err.Error(), "copy fallback")
		requireNoEntries(t, parent)
	})

	t.Run("failed write leaves the moved source in place", func(t *testing.T) {
		for name, content := range map[string][]byte{
			"not a tar":  []byte(strings.Repeat("x", 600)),
			"compressed": gzipBytes(t, data),
		} {
			src := writeSource(t, content)
			parent := t.TempDir()
			opts := linuxAMD64
			opts.MoveFile = true
			_, err := WriteFile(src, filepath.Join(parent, "layout"), opts)
			require.Error(t, err, name)
			assert.FileExists(t, src, name)
			requireNoEntries(t, parent)
		}

		src := writeSource(t, data)
		parent := t.TempDir()
		_, err := WriteFile(src, filepath.Join(parent, "layout"),
			Options{OS: "linux", Architecture: "amd64", MaxBytes: 10, MoveFile: true})
		require.ErrorIs(t, err, ErrTooLarge)
		assert.FileExists(t, src)

		require.NoError(t, os.MkdirAll(filepath.Join(parent, "layout", "x"), 0o755))
		_, err = WriteFile(src, filepath.Join(parent, "layout"), Options{OS: "linux", Architecture: "amd64", MoveFile: true})
		require.ErrorIs(t, err, ErrDestinationNotEmpty)
		assert.FileExists(t, src)
	})
}

func TestWriteFile_TrustedDigest(t *testing.T) {
	data := simpleTar(t)
	realDigest := digest.FromBytes(data)

	t.Run("matching digest gives the same layout as hashing", func(t *testing.T) {
		hashed := filepath.Join(t.TempDir(), "layout")
		want, err := WriteFile(writeSource(t, data), hashed, linuxAMD64)
		require.NoError(t, err)

		trusted := filepath.Join(t.TempDir(), "layout")
		opts := linuxAMD64
		opts.TrustedDigest = realDigest
		got, err := WriteFile(writeSource(t, data), trusted, opts)
		require.NoError(t, err)

		assert.Equal(t, want, got)
		assert.Equal(t, snapshot(t, hashed), snapshot(t, trusted))
	})

	t.Run("hashing is skipped", func(t *testing.T) {
		// A digest that does not match the content proves the file was not hashed: it is used verbatim.
		fake := digest.FromString("not the content")
		opts := linuxAMD64
		opts.TrustedDigest = fake

		dest := filepath.Join(t.TempDir(), "layout")
		_, err := WriteFile(writeSource(t, data), dest, opts)
		require.NoError(t, err)

		assert.FileExists(t, filepath.Join(dest, "blobs", "sha256", fake.Encoded()))
		assert.NoFileExists(t, filepath.Join(dest, "blobs", "sha256", realDigest.Encoded()))

		var index ocispec.Index
		readJSON(t, filepath.Join(dest, "index.json"), &index)
		var manifest ocispec.Manifest
		require.NoError(t, json.Unmarshal(verifyBlob(t, dest, index.Manifests[0]), &manifest))
		assert.Equal(t, fake, manifest.Layers[0].Digest)
		assert.Equal(t, int64(len(data)), manifest.Layers[0].Size)
		var cfg ocispec.Image
		require.NoError(t, json.Unmarshal(verifyBlob(t, dest, manifest.Config), &cfg))
		assert.Equal(t, []digest.Digest{fake}, cfg.RootFS.DiffIDs)
	})

	t.Run("size is still checked", func(t *testing.T) {
		opts := Options{OS: "linux", Architecture: "amd64", TrustedDigest: realDigest, MaxBytes: int64(len(data)) - 1}
		parent := t.TempDir()
		_, err := WriteFile(writeSource(t, data), filepath.Join(parent, "layout"), opts)
		require.ErrorIs(t, err, ErrTooLarge)
		requireNoEntries(t, parent)
	})

	t.Run("input is still validated", func(t *testing.T) {
		opts := linuxAMD64
		opts.TrustedDigest = realDigest
		parent := t.TempDir()
		_, err := WriteFile(writeSource(t, gzipBytes(t, data)), filepath.Join(parent, "layout"), opts)
		require.ErrorIs(t, err, ErrCompressed)
		requireNoEntries(t, parent)
	})

	t.Run("invalid digest", func(t *testing.T) {
		for name, d := range map[string]digest.Digest{
			"malformed":  "sha256:xyz",
			"no prefix":  digest.Digest(realDigest.Encoded()),
			"not sha256": digest.SHA512.FromBytes(data),
		} {
			opts := linuxAMD64
			opts.TrustedDigest = d
			parent := t.TempDir()
			_, err := WriteFile(writeSource(t, data), filepath.Join(parent, "layout"), opts)
			require.ErrorIs(t, err, ErrInvalidDigest, name)
			requireNoEntries(t, parent)
		}
	})

	t.Run("ignored by Write", func(t *testing.T) {
		opts := linuxAMD64
		opts.TrustedDigest = digest.FromString("not the content")
		dest := filepath.Join(t.TempDir(), "layout")
		_, err := Write(bytes.NewReader(data), dest, opts)
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(dest, "blobs", "sha256", realDigest.Encoded()))
	})
}
