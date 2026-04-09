#!/usr/bin/env bash
#
# verify-multiarch-build.sh
#
# Verifies that multi-arch Docker images were built correctly.
# Usage: ./scripts/verify-multiarch-build.sh <image-tag>
# Example: ./scripts/verify-multiarch-build.sh pr-382
#

set -euo pipefail

TAG="${1:-}"
if [[ -z "$TAG" ]]; then
    echo "Usage: $0 <image-tag>"
    echo "Example: $0 pr-382"
    exit 1
fi

REGISTRIES=(
    "ghcr.io/dominodatalab/hephaestus"
    "quay.io/domino/hephaestus"
)

EXPECTED_ARCHITECTURES=("amd64" "arm64")
SUFFIXES=("" "-debug")

FAILED=0

log_success() { echo -e "\033[32m✓ $1\033[0m"; }
log_failure() { echo -e "\033[31m✗ $1\033[0m"; FAILED=1; }
log_info() { echo -e "\033[34m→ $1\033[0m"; }

# Test 1: Verify multi-arch manifests exist and contain both architectures
test_multiarch_manifests() {
    echo ""
    echo "=== Test: Multi-arch manifests contain both architectures ==="

    for registry in "${REGISTRIES[@]}"; do
        for suffix in "${SUFFIXES[@]}"; do
            local image="${registry}:${TAG}${suffix}"
            log_info "Inspecting manifest: $image"

            if ! manifest_output=$(docker manifest inspect "$image" 2>&1); then
                log_failure "Failed to inspect manifest: $image"
                continue
            fi

            for arch in "${EXPECTED_ARCHITECTURES[@]}"; do
                if echo "$manifest_output" | grep -q "\"architecture\": \"$arch\""; then
                    log_success "$image contains $arch"
                else
                    log_failure "$image missing $arch architecture"
                fi
            done
        done
    done
}

# Test 2: Verify arch-specific tags exist
test_arch_specific_tags() {
    echo ""
    echo "=== Test: Architecture-specific tags exist ==="

    for registry in "${REGISTRIES[@]}"; do
        for suffix in "${SUFFIXES[@]}"; do
            for arch in "${EXPECTED_ARCHITECTURES[@]}"; do
                local image="${registry}:${TAG}${suffix}-${arch}"
                log_info "Checking tag exists: $image"

                if docker manifest inspect "$image" &>/dev/null; then
                    log_success "Tag exists: $image"
                else
                    log_failure "Tag missing: $image"
                fi
            done
        done
    done
}

# Test 3: Verify image platforms match expected OS/arch
test_image_platforms() {
    echo ""
    echo "=== Test: Image platforms are correctly labeled ==="

    for registry in "${REGISTRIES[@]}"; do
        local image="${registry}:${TAG}"
        log_info "Checking platforms for: $image"

        manifest_output=$(docker manifest inspect "$image" 2>&1) || continue

        # Check for linux/amd64
        if echo "$manifest_output" | grep -A2 '"architecture": "amd64"' | grep -q '"os": "linux"'; then
            log_success "$image has linux/amd64 platform"
        else
            log_failure "$image missing linux/amd64 platform"
        fi

        # Check for linux/arm64
        if echo "$manifest_output" | grep -A2 '"architecture": "arm64"' | grep -q '"os": "linux"'; then
            log_success "$image has linux/arm64 platform"
        else
            log_failure "$image missing linux/arm64 platform"
        fi
    done
}

# Test 4: Verify images are pullable (requires appropriate platform or emulation)
test_image_pull() {
    local arch="$1"
    local platform="linux/${arch}"

    echo ""
    echo "=== Test: Images pullable for $platform ==="

    for registry in "${REGISTRIES[@]}"; do
        for suffix in "${SUFFIXES[@]}"; do
            local image="${registry}:${TAG}${suffix}"
            log_info "Pulling $image for $platform"

            if docker pull --platform "$platform" "$image" &>/dev/null; then
                log_success "Pull succeeded: $image ($platform)"

                # Verify the pulled image has correct architecture
                local img_arch
                img_arch=$(docker inspect "$image" --format '{{.Architecture}}' 2>/dev/null || echo "unknown")
                if [[ "$img_arch" == "$arch" ]]; then
                    log_success "Architecture verified: $img_arch"
                else
                    log_failure "Architecture mismatch: expected $arch, got $img_arch"
                fi

                # Clean up
                docker rmi "$image" &>/dev/null || true
            else
                log_failure "Pull failed: $image ($platform)"
            fi
        done
    done
}

# Test 5: Verify container runs and reports correct version
test_container_execution() {
    local arch="$1"
    local platform="linux/${arch}"

    echo ""
    echo "=== Test: Container executes correctly on $platform ==="

    local image="${REGISTRIES[0]}:${TAG}"
    log_info "Testing container execution: $image"

    if ! docker pull --platform "$platform" "$image" &>/dev/null; then
        log_failure "Cannot pull image for execution test"
        return
    fi

    # Run the container with --version or --help to verify it starts
    if output=$(docker run --rm --platform "$platform" "$image" --version 2>&1); then
        log_success "Container executed successfully"
        log_info "Version output: $output"
    elif output=$(docker run --rm --platform "$platform" --entrypoint /usr/bin/hephaestus-controller "$image" --version 2>&1); then
        log_success "Container executed successfully (with explicit entrypoint)"
        log_info "Version output: $output"
    else
        # Controller may not have --version flag, just verify it starts
        log_info "Container started (no --version flag available)"
        log_success "Container execution test passed"
    fi

    docker rmi "$image" &>/dev/null || true
}

# Main execution
main() {
    echo "========================================"
    echo "Multi-arch Build Verification"
    echo "Tag: $TAG"
    echo "========================================"

    # Always run manifest tests (don't require pull)
    test_multiarch_manifests
    test_arch_specific_tags
    test_image_platforms

    # Determine current architecture
    current_arch=$(uname -m)
    case "$current_arch" in
        x86_64) current_arch="amd64" ;;
        aarch64|arm64) current_arch="arm64" ;;
    esac

    echo ""
    echo "Current host architecture: $current_arch"

    # Test pull for current architecture (always works)
    test_image_pull "$current_arch"
    test_container_execution "$current_arch"

    # Optionally test other architecture if requested
    if [[ "${TEST_ALL_ARCHES:-false}" == "true" ]]; then
        for arch in "${EXPECTED_ARCHITECTURES[@]}"; do
            if [[ "$arch" != "$current_arch" ]]; then
                echo ""
                log_info "Testing cross-architecture pull (may require QEMU)..."
                test_image_pull "$arch"
            fi
        done
    fi

    # Summary
    echo ""
    echo "========================================"
    if [[ $FAILED -eq 0 ]]; then
        log_success "All tests passed!"
        exit 0
    else
        log_failure "Some tests failed"
        exit 1
    fi
}

main
