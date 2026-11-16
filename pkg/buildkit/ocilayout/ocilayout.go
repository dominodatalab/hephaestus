// Package ocilayout wraps a tar archive into a single-layer OCI image layout directory.
//
// The resulting layout (oci-layout, index.json, blobs/sha256/...) can be consumed as an OCI layout
// source, for example as a BuildKit named context. The package is pure: it only reads the supplied
// tar and writes under the supplied directory.
//
// Design choices:
//   - The layer is stored UNCOMPRESSED (media type application/vnd.oci.image.layer.v1.tar). This keeps
//     the output byte-for-byte deterministic without depending on a gzip implementation, and the
//     layer digest equals the diff_id. The input tar bytes are stored verbatim, so determinism of the
//     layout is exactly the determinism of the input tar. Because the layer is declared uncompressed,
//     compressed input (gzip, zstd, xz) is rejected with ErrCompressed: it would produce a diff_id that
//     does not match the uncompressed content.
//   - The image config is minimal: architecture/os/variant, a rootfs of type "layers" with one diff_id,
//     and no timestamps, history or runtime config. The platform is REQUIRED (Options.OS and
//     Options.Architecture, optional Options.Variant), with no default. BuildKit resolves an OCI layout
//     named context using the platform of the build, and this repository performs multi-arch builds, so
//     a silent default would select or reject the wrong image. The same platform is also recorded on
//     the manifest descriptor in index.json.
//   - The input is validated by peeking at its first 512-byte block only (the stream is never
//     buffered): that block must be all zeros (a valid empty archive) or a tar header carrying the
//     ustar or GNU magic and a correct header checksum, otherwise ErrNotTar is returned. Archives in
//     the original V7 format, which has no magic, are therefore rejected. Zero-byte input returns
//     ErrEmpty and input larger than Options.MaxBytes returns ErrTooLarge.
//   - WriteFile avoids storing the tar twice. After hashing the file (or trusting
//     Options.TrustedDigest) the file is moved into blobs/ with os.Rename when Options.MoveFile is
//     set, or hard-linked with os.Link otherwise, falling back to a plain copy whenever the rename
//     or link fails (for example across filesystems or on filesystems without hard links). Write,
//     which consumes an io.Reader, always streams into the layout.
//   - Writes are atomic. The layout is staged in a temporary directory created next to the destination
//     and renamed into place on success; the destination must not exist or must be an empty
//     directory (otherwise ErrDestinationNotEmpty). The staging directory is removed on any error, so
//     a failed Write never leaves a half-written layout behind. Staging next to the destination
//     keeps the final rename on one filesystem; for rename/link placement the source tar must be on
//     the same filesystem as the destination's parent directory, otherwise the copy fallback is used.
package ocilayout

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// DefaultMaxBytes is the default cap on the input tar size (2 GiB).
	DefaultMaxBytes int64 = 2 << 30

	dirPerm  = 0o755
	filePerm = 0o644

	// tarBlockSize is the size of a tar header block, and the amount of input peeked for validation.
	tarBlockSize = 512
	// copyBufSize is the read buffer used for streaming; it must be at least tarBlockSize.
	copyBufSize = 32 << 10

	// Offsets and sizes of the tar header fields used for validation.
	tarChecksumOffset = 148
	tarChecksumLen    = 8
	tarMagicOffset    = 257
	tarMagic          = "ustar"
)

var (
	// ErrEmpty is returned when the input contains no bytes at all.
	ErrEmpty = errors.New("ocilayout: tar input is empty")
	// ErrTooLarge is returned when the input exceeds Options.MaxBytes.
	ErrTooLarge = errors.New("ocilayout: tar input exceeds size limit")
	// ErrPlatformRequired is returned when Options.OS or Options.Architecture is empty.
	ErrPlatformRequired = errors.New("ocilayout: platform OS and architecture are required")
	// ErrCompressed is returned when the input is gzip, zstd or xz compressed; the layer is stored
	// uncompressed, so only an uncompressed tar is accepted.
	ErrCompressed = errors.New("ocilayout: tar input is compressed, an uncompressed tar is required")
	// ErrNotTar is returned when the input is neither an empty tar archive nor starts with a valid
	// tar header.
	ErrNotTar = errors.New("ocilayout: input is not a tar archive")
	// ErrDestinationNotEmpty is returned when the destination exists and is not an empty directory.
	ErrDestinationNotEmpty = errors.New("ocilayout: destination exists and is not an empty directory")
	// ErrInvalidDigest is returned when Options.TrustedDigest is not a valid sha256 digest.
	ErrInvalidDigest = errors.New("ocilayout: trusted digest is not a valid sha256 digest")
)

// Seams for the filesystem operations used to place a file into the layout, so tests can force the
// copy fallback.
var (
	linkFile   = os.Link
	renameFile = os.Rename
)

// compressionMagics are the magic numbers of the compression formats rejected with ErrCompressed.
var compressionMagics = []struct {
	name  string
	magic []byte
}{
	{"gzip", []byte{0x1f, 0x8b}},
	{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
	{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
}

// Options tunes Write and WriteFile. OS and Architecture are required; everything else is optional.
type Options struct {
	// OS and Architecture set the image config platform (for example "linux" and "arm64"). Both are
	// required: Write returns ErrPlatformRequired when either is empty. There is no default, because
	// BuildKit matches an OCI layout named context against the platform of the build.
	OS           string
	Architecture string
	// Variant is the optional platform variant (for example "v8" for arm64).
	Variant string

	// MaxBytes caps the number of tar bytes accepted. Zero means DefaultMaxBytes.
	MaxBytes int64

	// TrustedDigest, honored by WriteFile only, is the sha256 digest of the tar file that the caller
	// has already verified (for example the digest returned by a verifying download). When set,
	// WriteFile does not hash the file. The size is still checked against MaxBytes and the first block
	// is still validated. The digest is used as-is for the layer blob and the diff_id: a wrong value
	// produces an invalid layout, so only pass a digest that was actually verified against the file's
	// content. Write always hashes the stream and ignores this field.
	TrustedDigest digest.Digest

	// MoveFile, honored by WriteFile only, means the source file is consumed: it is moved into the
	// layout with os.Rename instead of being hard-linked, and with the copy fallback the source is
	// removed after the copy. On error the source is normally left in place, but callers should not
	// rely on it once MoveFile is set. Write ignores this field.
	MoveFile bool
}

// layer is a validated and digested layer whose blob is yet to be placed into the layout. Placement
// is the last step before the layout is committed, so a failure earlier never consumes the source.
type layer struct {
	desc  ocispec.Descriptor
	place func(blobDir string) error
}

// WriteFile is like Write but reads the tar from the regular file at path.
//
// Instead of copying the file into the layout, WriteFile places it into blobs/ by hard link (or by
// rename with Options.MoveFile), which avoids doubling the disk usage for large archives, and falls
// back to a copy when that fails (for example across filesystems). Because the blob and the source
// share an inode when linked or moved:
//   - the source must not be modified or truncated afterwards, as that would corrupt the layout;
//   - the blob is made world-readable (mode 0644), so with a hard link the source file's mode changes
//     to 0644 as well.
//
// For rename and link to apply, the source must be on the same filesystem as the parent directory of
// dir, where the layout is staged.
func WriteFile(path, dir string, opts Options) (digest.Digest, error) {
	return write(dir, opts, func(_ string, maxBytes int64) (layer, error) {
		return fileLayer(path, maxBytes, opts)
	})
}

// Write wraps the tar stream r into a one-layer OCI layout rooted at dir and returns the image
// manifest digest.
//
// dir must not exist or must be an empty directory (else ErrDestinationNotEmpty); missing parent
// directories are created. The layout is staged in a temporary directory next to dir and renamed
// into place, so on error nothing is left at dir and the staging directory is removed. The stream is
// copied into the layout; use WriteFile to avoid the copy when the tar is already a file on disk.
func Write(r io.Reader, dir string, opts Options) (digest.Digest, error) {
	return write(dir, opts, func(blobDir string, maxBytes int64) (layer, error) {
		return streamLayer(r, blobDir, maxBytes)
	})
}

// write stages a layout built around the layer produced by newLayer and commits it to dir.
func write(
	dir string,
	opts Options,
	newLayer func(blobDir string, maxBytes int64) (layer, error),
) (_ digest.Digest, err error) {
	if opts.OS == "" || opts.Architecture == "" {
		return "", ErrPlatformRequired
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	dir = filepath.Clean(dir)
	// Fail fast, before consuming the input.
	if err := checkDestination(dir); err != nil {
		return "", err
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, dirPerm); err != nil {
		return "", fmt.Errorf("ocilayout: create parent dir: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".ocilayout-*")
	if err != nil {
		return "", fmt.Errorf("ocilayout: create staging dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	// MkdirTemp creates the directory with mode 0700, but the final layout must be readable.
	if err := os.Chmod(stage, dirPerm); err != nil {
		return "", fmt.Errorf("ocilayout: chmod staging dir: %w", err)
	}

	blobDir := filepath.Join(stage, "blobs", string(digest.SHA256))
	if err := os.MkdirAll(blobDir, dirPerm); err != nil {
		return "", fmt.Errorf("ocilayout: create blob dir: %w", err)
	}

	l, err := newLayer(blobDir, maxBytes)
	if err != nil {
		return "", err
	}

	platform := ocispec.Platform{Architecture: opts.Architecture, OS: opts.OS, Variant: opts.Variant}
	config := ocispec.Image{
		Platform: platform,
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{l.desc.Digest}},
	}
	configDesc, err := writeJSONBlob(blobDir, ocispec.MediaTypeImageConfig, config)
	if err != nil {
		return "", err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{l.desc},
	}
	manifestDesc, err := writeJSONBlob(blobDir, ocispec.MediaTypeImageManifest, manifest)
	if err != nil {
		return "", err
	}
	// Record the platform on the index entry too, so consumers can select the manifest without
	// having to read the config.
	manifestDesc.Platform = &platform

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifestDesc},
	}
	if err := writeJSONFile(filepath.Join(stage, ocispec.ImageIndexFile), index); err != nil {
		return "", err
	}

	layout := ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion}
	if err := writeJSONFile(filepath.Join(stage, ocispec.ImageLayoutFile), layout); err != nil {
		return "", err
	}

	// Placing the layer blob is deferred to here so that a move never consumes the source for a
	// layout that fails to build.
	if err := l.place(blobDir); err != nil {
		return "", err
	}

	if err := commit(stage, dir); err != nil {
		return "", err
	}

	return manifestDesc.Digest, nil
}

// checkDestination returns ErrDestinationNotEmpty unless dir does not exist or is an empty directory.
func checkDestination(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ocilayout: stat destination: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrDestinationNotEmpty, dir)
	}

	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("ocilayout: open destination: %w", err)
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s", ErrDestinationNotEmpty, dir)
	case errors.Is(err, io.EOF):
		return nil
	default:
		return fmt.Errorf("ocilayout: read destination: %w", err)
	}
}

// commit renames the staged layout into place, replacing dir if it is an empty directory.
func commit(stage, dir string) error {
	// Re-check: dir may have appeared or been filled while the layout was being built. The removal
	// below only ever applies to an empty directory, and fails on a non-empty one.
	if err := checkDestination(dir); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrDestinationNotEmpty, dir)
		}

		return fmt.Errorf("ocilayout: remove empty destination: %w", err)
	}
	if err := os.Rename(stage, dir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrDestinationNotEmpty, dir)
		}

		return fmt.Errorf("ocilayout: commit layout: %w", err)
	}

	return nil
}

// streamLayer validates r, streams it into a temp file inside blobDir while hashing, and returns a
// layer that renames the temp file to its digest.
func streamLayer(r io.Reader, blobDir string, maxBytes int64) (layer, error) {
	br := bufio.NewReaderSize(r, copyBufSize)
	if err := checkHead(br); err != nil {
		return layer{}, err
	}

	tmp, err := os.CreateTemp(blobDir, ".layer-*")
	if err != nil {
		return layer{}, fmt.Errorf("ocilayout: create temp layer: %w", err)
	}
	tmpName := tmp.Name()
	// The staging directory is removed wholesale on error, so the temp file needs no cleanup of its own.
	defer tmp.Close()

	h := sha256.New()
	// Read one byte past the cap so an oversized input is detected rather than silently truncated.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(br, maxBytes+1))
	if err != nil {
		return layer{}, fmt.Errorf("ocilayout: copy tar: %w", err)
	}
	if n == 0 {
		return layer{}, ErrEmpty
	}
	if n > maxBytes {
		return layer{}, fmt.Errorf("%w (limit %d bytes)", ErrTooLarge, maxBytes)
	}
	if err := tmp.Close(); err != nil {
		return layer{}, fmt.Errorf("ocilayout: close temp layer: %w", err)
	}

	dgst := digest.NewDigestFromBytes(digest.SHA256, h.Sum(nil))

	return layer{
		desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: dgst, Size: n},
		place: func(blobDir string) error {
			if err := os.Chmod(tmpName, filePerm); err != nil {
				return fmt.Errorf("ocilayout: chmod layer: %w", err)
			}
			if err := os.Rename(tmpName, filepath.Join(blobDir, dgst.Encoded())); err != nil {
				return fmt.Errorf("ocilayout: commit layer: %w", err)
			}

			return nil
		},
	}, nil
}

// fileLayer validates and digests the tar file at path and returns a layer that places the file
// itself into the blob directory.
func fileLayer(path string, maxBytes int64, opts Options) (layer, error) {
	f, err := os.Open(path)
	if err != nil {
		return layer{}, fmt.Errorf("ocilayout: open tar: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return layer{}, fmt.Errorf("ocilayout: stat tar: %w", err)
	}
	if !info.Mode().IsRegular() {
		return layer{}, fmt.Errorf("ocilayout: tar input %s is not a regular file", path)
	}
	size := info.Size()
	if size == 0 {
		return layer{}, ErrEmpty
	}
	if size > maxBytes {
		return layer{}, fmt.Errorf("%w (limit %d bytes)", ErrTooLarge, maxBytes)
	}

	br := bufio.NewReaderSize(f, copyBufSize)
	if err := checkHead(br); err != nil {
		return layer{}, err
	}

	dgst := opts.TrustedDigest
	if dgst != "" {
		// Validate first: Algorithm panics on a digest without an algorithm separator.
		if dgst.Validate() != nil || dgst.Algorithm() != digest.SHA256 {
			return layer{}, fmt.Errorf("%w: %q", ErrInvalidDigest, dgst)
		}
	} else {
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(br, size+1))
		if err != nil {
			return layer{}, fmt.Errorf("ocilayout: hash tar: %w", err)
		}
		if n != size {
			return layer{}, fmt.Errorf("ocilayout: tar input %s changed while being read", path)
		}
		dgst = digest.NewDigestFromBytes(digest.SHA256, h.Sum(nil))
	}
	// Close before placing; the placement reopens the file when it has to copy.
	_ = f.Close()

	return layer{
		desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: dgst, Size: size},
		place: func(blobDir string) error {
			return placeFile(path, filepath.Join(blobDir, dgst.Encoded()), opts.MoveFile)
		},
	}, nil
}

// placeFile puts the file src at dst by rename (move) or hard link, falling back to a copy if that
// fails for any reason (typically a cross-device link, or a filesystem without hard links), and
// leaves dst with mode 0644. With move, the source is gone afterwards.
func placeFile(src, dst string, move bool) error {
	place := linkFile
	if move {
		place = renameFile
	}

	if placeErr := place(src, dst); placeErr != nil {
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("ocilayout: place layer: %w (copy fallback after %w)", err, placeErr)
		}
		if move {
			if err := os.Remove(src); err != nil {
				return fmt.Errorf("ocilayout: remove moved source: %w", err)
			}
		}
	}

	// For a link or rename this is the source's inode, so its mode changes too.
	if err := os.Chmod(dst, filePerm); err != nil {
		return fmt.Errorf("ocilayout: chmod layer: %w", err)
	}

	return nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	_, err = io.Copy(out, in)

	return err
}

// checkHead peeks at the first block of the input without consuming it and rejects empty,
// compressed and non-tar input.
func checkHead(br *bufio.Reader) error {
	head, err := br.Peek(tarBlockSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("ocilayout: read tar: %w", err)
	}
	if len(head) == 0 {
		return ErrEmpty
	}
	for _, c := range compressionMagics {
		if bytes.HasPrefix(head, c.magic) {
			return fmt.Errorf("%w (%s)", ErrCompressed, c.name)
		}
	}
	if len(head) < tarBlockSize {
		return fmt.Errorf("%w: input is shorter than one %d-byte block", ErrNotTar, tarBlockSize)
	}
	// An all-zero first block is the start of a valid empty archive.
	if isZero(head) {
		return nil
	}
	if !validTarHeader(head) {
		return fmt.Errorf("%w: first block is not a ustar or GNU tar header with a valid checksum", ErrNotTar)
	}

	return nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}

	return true
}

// validTarHeader reports whether block is a ustar/GNU tar header with a correct checksum.
func validTarHeader(block []byte) bool {
	if len(block) < tarBlockSize {
		return false
	}
	// ustar is "ustar\0" followed by version "00"; GNU is "ustar " followed by " \0".
	if string(block[tarMagicOffset:tarMagicOffset+len(tarMagic)]) != tarMagic {
		return false
	}
	if sep := block[tarMagicOffset+len(tarMagic)]; sep != 0 && sep != ' ' {
		return false
	}

	field := bytes.Trim(block[tarChecksumOffset:tarChecksumOffset+tarChecksumLen], " \x00")
	want, err := strconv.ParseInt(string(field), 8, 64)
	if err != nil {
		return false
	}

	// The checksum is computed with the checksum field itself taken as spaces. Some historic
	// implementations summed signed bytes, so accept either.
	var unsigned, signed int64
	for i, c := range block {
		if i >= tarChecksumOffset && i < tarChecksumOffset+tarChecksumLen {
			c = ' '
		}
		unsigned += int64(c)
		signed += int64(int8(c))
	}

	return want == unsigned || want == signed
}

func writeJSONBlob(blobDir, mediaType string, v any) (ocispec.Descriptor, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("ocilayout: marshal %s: %w", mediaType, err)
	}
	dgst := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(blobDir, dgst.Encoded()), data, filePerm); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("ocilayout: write blob: %w", err)
	}

	return ocispec.Descriptor{MediaType: mediaType, Digest: dgst, Size: int64(len(data))}, nil
}

func writeJSONFile(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ocilayout: marshal %s: %w", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("ocilayout: write %s: %w", filepath.Base(path), err)
	}

	return nil
}
