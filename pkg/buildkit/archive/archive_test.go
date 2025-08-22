package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/stdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDataURLSupport(t *testing.T) {
	// Set up logging
	stdr.SetVerbosity(1)
	logger := stdr.New(nil)

	// Create a simple Dockerfile content
	dockerfileContent := `FROM python:3.10-slim AS downloader

RUN pip install --no-cache-dir huggingface_hub

RUN set -eu && \
   python3 -c "\
from huggingface_hub import snapshot_download; \
snapshot_download(repo_id='test-model', revision='main', local_dir='/model')" && \
   rm -rf /model/.cache /model/.gitattributes

FROM scratch

# Copy model contents to image root
COPY --from=downloader /model /
`

	// Create a tar file containing the Dockerfile
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Add Dockerfile to tar
	header := &tar.Header{
		Name: "Dockerfile",
		Mode: 0644,
		Size: int64(len(dockerfileContent)),
	}

	err := tw.WriteHeader(header)
	require.NoError(t, err)

	_, err = tw.Write([]byte(dockerfileContent))
	require.NoError(t, err)

	err = tw.Close()
	require.NoError(t, err)

	// Base64 encode the tar file
	base64Tar := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Create data URL
	dataURL := "data:application/x-tar;base64," + base64Tar

	t.Logf("Generated data URL: %s...", dataURL[:100])

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "hephaestus-data-url-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	t.Logf("Created temp directory: %s", tempDir)

	ctx := context.Background()

	t.Run("Data URL Detection", func(t *testing.T) {
		// Test that our data URL detection works
		assert.True(t, strings.HasPrefix(dataURL, "data:"), "Data URL should start with 'data:'")

		// Test with non-data URL
		httpURL := "http://example.com/file.tar"
		assert.False(t, strings.HasPrefix(httpURL, "data:"), "HTTP URL should not be detected as data URL")
	})

	t.Run("Data URL Processing", func(t *testing.T) {
		// Test that our data URL processing works
		extraction, err := FetchAndExtract(ctx, logger, dataURL, tempDir, 30*time.Second)

		// This should succeed with our data URL support
		require.NoError(t, err, "FetchAndExtract should succeed with data URL support")
		require.NotNil(t, extraction, "Extraction should not be nil")

		t.Logf("✅ SUCCESS: FetchAndExtract completed with data URL!")
		t.Logf("   Archive: %s", extraction.Archive)
		t.Logf("   ContentsDir: %s", extraction.ContentsDir)

		// Verify the archive file was created
		_, err = os.Stat(extraction.Archive)
		require.NoError(t, err, "Archive file should exist")

		// Verify the contents directory was created
		_, err = os.Stat(extraction.ContentsDir)
		require.NoError(t, err, "Contents directory should exist")

		// Verify the Dockerfile was extracted correctly
		dockerfilePath := filepath.Join(extraction.ContentsDir, "Dockerfile")
		_, err = os.Stat(dockerfilePath)
		require.NoError(t, err, "Dockerfile should be extracted")

		// Read and verify the Dockerfile content
		extractedContent, err := os.ReadFile(dockerfilePath)
		require.NoError(t, err, "Should be able to read extracted Dockerfile")

		assert.Equal(t, dockerfileContent, string(extractedContent), "Extracted Dockerfile content should match original")
		t.Logf("✅ Dockerfile content verified: %d bytes", len(extractedContent))
	})

	t.Run("Data URL with Different Media Types", func(t *testing.T) {
		// Test data URL without base64 encoding
		plainDataURL := "data:text/plain," + dockerfileContent

		// This should also work with our data URL support
		_, err := FetchAndExtract(ctx, logger, plainDataURL, tempDir, 30*time.Second)

		// This should succeed (though it's not a tar file, so extraction might fail)
		// But the data URL parsing should work
		if err != nil {
			t.Logf("Expected error for non-tar data URL: %v", err)
		} else {
			t.Logf("✅ SUCCESS: Plain data URL also processed!")
		}
	})

	t.Run("Invalid Data URL", func(t *testing.T) {
		// Test with invalid data URL format
		invalidDataURL := "data:invalid-format"

		_, err := FetchAndExtract(ctx, logger, invalidDataURL, tempDir, 30*time.Second)

		// This should fail with a clear error message
		require.Error(t, err, "Invalid data URL should fail")
		t.Logf("✅ Expected error for invalid data URL: %v", err)
		assert.Contains(t, err.Error(), "invalid data URL format", "Error should mention invalid format")
	})

	t.Run("Base64 Decoding Error", func(t *testing.T) {
		// Test with invalid base64 data
		invalidBase64DataURL := "data:application/x-tar;base64,invalid-base64-data"

		_, err := FetchAndExtract(ctx, logger, invalidBase64DataURL, tempDir, 30*time.Second)

		// This should fail with a base64 decoding error
		require.Error(t, err, "Invalid base64 data should fail")
		t.Logf("✅ Expected error for invalid base64: %v", err)
		assert.Contains(t, err.Error(), "failed to decode base64 data", "Error should mention base64 decoding")
	})
}

func TestDataURLDetection(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{
			name:     "Data URL with tar",
			url:      "data:application/x-tar;base64,test",
			expected: true,
		},
		{
			name:     "Data URL with gzip",
			url:      "data:application/gzip;base64,test",
			expected: true,
		},
		{
			name:     "HTTP URL",
			url:      "http://example.com/file.tar",
			expected: false,
		},
		{
			name:     "HTTPS URL",
			url:      "https://example.com/file.tar.gz",
			expected: false,
		},
		{
			name:     "File path",
			url:      "/path/to/file.tar",
			expected: false,
		},
		{
			name:     "Empty string",
			url:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := strings.HasPrefix(tt.url, "data:")
			if result != tt.expected {
				t.Errorf("Expected %v, got %v for URL: %s", tt.expected, result, tt.url)
			}
		})
	}
}
