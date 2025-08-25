# Data URL Support for Hephaestus

## Problem Statement

Hugging Face model image building needed to send Dockerfiles to Hephaestus for OCI image creation. Model image
builds require only a single, small Dockerfile that contains the instructions to download and package a specific model
from Hugging Face Hub.

### Why Data URLs Instead of HTTP Endpoints?

The initial approach was to create an internal HTTP endpoint to serve Dockerfiles, but this was overkill for the use case:

- **Single Small File**: Hugging Face model image builds only need one small Dockerfile
- **No Persistence Required**: The Dockerfile is generated on-demand and doesn't need to be stored
- **Simplified Architecture**: Avoids the complexity of routes, controllers, and internal API endpoints

### What Are Data URLs?

Data URLs are a way to embed small files directly within a URL string. They follow the format:
```
data:[<mediatype>][;base64],<content>
```

**How Data URLs Simplify the Design**:

Hephaestus requires the client to provide a source URL that it will call back during build processing to fetch a tar 
archive of project files for environment or model API builds.  For model image builds, we offer data URLs instead of
HTTP URLs.

1. **Self-Contained**: The Dockerfile content is embedded directly in the data URL
2. **No Network Overhead**: Eliminates need for internal HTTP endpoints to serve Dockerfiles
3. **Direct Integration**: The Hugging Face model image builder can generate and send the complete build context directly to Hephaestus without callbacks

### Problems Encountered

When attempting to use data URLs with Hephaestus, the build failed with a misleading error message:

```
cannot fetch remote context: stat : no such file or directory
```

**Investigation revealed two issues:**

1. **Misleading Error Message**: The error handling in `buildkit.go` used the wrong error variable and logged a misleading error message

2. **Missing Data URL Support**: Hephaestus's `archive.FetchAndExtract` only supports HTTP URLs and attempts to make HTTP requests against data URLs, resulting in "unsupported protocol scheme 'data'" errors.

## Solution Implemented

### 1. Fixed Misleading Error Message

**File**: `pkg/buildkit/buildkit.go`

**Issue**: Line 184 incorrectly used `err` instead of `extractErr`:
```go
    // Original (incorrect)
    extract, extractErr := archive.FetchAndExtract(ctx, c.log, opts.Context, buildDir, opts.FetchAndExtractTimeout)
    if extractErr != nil {
        return "", fmt.Errorf("cannot fetch remote context: %w", err)
    }
```

### 2. Data URL Support in Archive Package

**File**: `pkg/buildkit/archive/archive.go`

**Changes**:
- Modified `FetchAndExtract` to detect data URLs using `strings.HasPrefix(url, "data:")`
- Added `downloadDataURL` function using an RFC 2397 compliant library for parsing
- Maintained backward compatibility with existing HTTP/HTTPS URL support

**Key Features**:
- Full support for all data URL formats
- Library handles base64 decoding and media type parsing
- Provides detailed logging and error handling

### 3. Comprehensive Test Coverage

**File**: `pkg/buildkit/archive/archive_test.go`

**Tests Added**:
- `TestDataURLSupport`: Validates data URL detection, processing, and error handling
- `TestDataURLDetection`: Ensures backward compatibility with existing URL types
- Error scenarios: Invalid data URL format, base64 decoding errors, non-tar content

**Test Coverage**:
- Data URL detection logic
- RFC 2397 compliant parsing via dataurl library
- Error handling for invalid inputs
- Backward compatibility with HTTP/HTTPS URLs
- Content verification (Dockerfile extraction and validation)

### 4. RFC 2397 Compliance

**Library Choice**: `github.com/lestrrat-go/dataurl`

**Supported Formats**:
- `data:,<content>` (no media type)
- `data:;base64,<content>` (with base64)
- `data:text/plain,<content>` (with media type)
- `data:application/x-tar;base64,<content>` (with media type and base64)
- `data:text/plain;charset=utf-8;base64,<content>` (with media type, parameters and base64)

## Usage

The Hugging Face model image builder now generates data URLs containing base64-encoded tar files with Dockerfiles:

```scala
// Example data URL format
"data:application/x-tar;base64,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AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
```

Hephaestus now successfully processes these data URLs and extracts the Dockerfile for image building.

## Data URL Size Considerations
Our data URLs are extremely small (~1 KB) and well within all practical limits.  The data URL size is not a concern because:

1. **Kubernetes API Transport**: Data URLs are passed via Kubernetes API calls (gRPC/HTTP2), not HTTP requests
2. **No HTTP Limits**: We avoid nginx, Node.js, or web server URL length restrictions
3. **K8s API Limits**: etcd enforces a gRPC message size limit of 1.5 MB for API server requests and responses
4. **CRD Field Storage**: The data URL is stored as a field in the ImageBuild CRD spec

## Testing

All tests pass and demonstrate:
- RFC 2397 compliant data URL processing via dataurl library
- Error handling is robust
- Backward compatibility maintained
- Content integrity preserved

Run tests with:
```bash
go test -v ./pkg/buildkit/archive
```
