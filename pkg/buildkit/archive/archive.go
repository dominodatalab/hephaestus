package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/h2non/filetype"
	"github.com/lestrrat-go/dataurl"
	"k8s.io/apimachinery/pkg/util/wait"
)

type mimeType string

const (
	mimeTypeTar  = mimeType("application/x-tar")
	mimeTypeGzip = mimeType("application/gzip")
)

var defaultBackoff = wait.Backoff{ // retries after 1s 2s 4s 8s 16s 32s 64s 128s with jitter
	Duration: time.Second,
	Factor:   2,
	Jitter:   0.1,
	Steps:    8,
}

type fileDownloader interface {
	Do(req *http.Request) (*http.Response, error)
}

type Extractor func(context.Context, logr.Logger, string, string, time.Duration) (*Extraction, error)

type Extraction struct {
	Archive     string
	ContentsDir string
}

func AssertDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}

	return nil
}

func FetchAndExtract(ctx context.Context, log logr.Logger, url, wd string, timeout time.Duration) (*Extraction, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := AssertDir(wd); err != nil {
		return nil, fmt.Errorf("invalid build context directory: %w", err)
	}

	archive := filepath.Join(wd, "archive")

	// Check if the URL is a data URL
	if strings.HasPrefix(url, "data:") {
		err := downloadDataURL(ctx, log, url, archive)
		if err != nil {
			return nil, err
		}
	} else {
		// Handle HTTP URLs with retry logic
		err := wait.ExponentialBackoffWithContext(ctx, defaultBackoff, func(ctx context.Context) (bool, error) {
			return downloadFile(ctx, log, http.DefaultClient, url, archive)
		})
		if err != nil {
			return nil, err
		}
	}

	ct, err := getFileContentType(archive)
	if err != nil {
		return nil, err
	}
	if ct != mimeTypeGzip && ct != mimeTypeTar {
		return nil, fmt.Errorf("unsupported file content type %q", ct)
	}

	dest := filepath.Join(wd, "extracted")
	if err := os.MkdirAll(dest, 0755); err != nil {
		return nil, err
	}
	if err := extract(archive, ct, dest); err != nil {
		return nil, err
	}

	return &Extraction{
		Archive:     archive,
		ContentsDir: dest,
	}, nil
}

func downloadDataURL(_ context.Context, log logr.Logger, dataURL, fp string) error {
	// Parse using RFC 2397 compliant library
	parsed, err := dataurl.Parse([]byte(dataURL))
	if err != nil {
		return fmt.Errorf("failed to parse data URL: %w", err)
	}

	log.Info("Processing data URL", "mediaType", parsed.MediaType.Type, "dataLength", len(parsed.Data))

	// Write the data to the archive file
	out, err := os.Create(fp)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer out.Close()

	_, err = out.Write(parsed.Data)
	if err != nil {
		return fmt.Errorf("failed to write archive file: %w", err)
	}

	log.Info("Successfully wrote data URL content to archive file", "file", fp, "size", len(parsed.Data))
	return nil
}

func retryable(err *url.Error) bool {
	// If we get any sort of operational error before an HTTP response we retry it.
	var opError *net.OpError
	return err.Timeout() || err.Temporary() || errors.As(err, &opError)
}

// downloadFile takes a file URL and local location to download it to.
// It returns "done" (retryable or not) and an error.
func downloadFile(ctx context.Context, log logr.Logger, c fileDownloader, fileURL, fp string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.Do(req)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) && retryable(urlError) {
			log.Error(
				urlError, "Received temporary or transient error while fetching context, will attempt to retry",
				"url", fileURL, "file", fp,
			)
			return false, nil
		}

		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusServiceUnavailable:
		log.Info(
			"Received transient status code while fetching context, will attempt to retry",
			"url", fileURL, "file", fp, "code", resp.StatusCode,
		)
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("file download failed with status %d", resp.StatusCode)
	}

	out, err := os.Create(fp)
	if err != nil {
		return false, err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return true, err
}

func getFileContentType(fp string) (ct mimeType, err error) {
	f, err := os.Open(fp)
	if err != nil {
		return
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err = f.Read(buf); err != nil {
		return
	}

	kind, err := filetype.Match(buf)
	if err != nil {
		return
	}

	return mimeType(kind.MIME.Value), nil
}

func extract(fp string, ct mimeType, dst string) error {
	f, err := os.Open(fp)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader
	if ct == mimeTypeGzip {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gzr.Close()

		r = gzr
	} else {
		r = bufio.NewReader(f)
	}

	tr := tar.NewReader(r)

	for {
		header, err := tr.Next()

		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return err
		case header == nil:
			continue
		}

		target, err := sanitizeExtractPath(dst, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err = copyRegularFile(target, tr, header.Mode); err != nil {
				return err
			}
		}
	}
}

func sanitizeExtractPath(destination, filename string) (string, error) {
	destPath := filepath.Join(destination, filename)
	if !strings.HasPrefix(destPath, filepath.Clean(destination)) {
		return "", fmt.Errorf("content filepath tainted: %s", destPath)
	}

	return destPath, nil
}

func copyRegularFile(target string, tr *tar.Reader, mode int64) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(mode))
	if err != nil {
		return err
	}
	defer f.Close()

	for {
		if _, err = io.CopyN(f, tr, 1024); err != nil {
			if err == io.EOF {
				break
			}

			return fmt.Errorf("error reading tar regular file: %w", err)
		}
	}

	return nil
}
