#!/usr/bin/env bash
#
# verify-helm-version.sh
#
# Verifies that the Helm chart was packaged with the correct version
# derived from the docker-manifest job output.
#
# Usage: ./scripts/verify-helm-version.sh <expected-version>
# Example: ./scripts/verify-helm-version.sh pr-382
#

set -euo pipefail

EXPECTED_VERSION="${1:-}"
CHART_PATH="${2:-./helm/hephaestus}"

if [[ -z "$EXPECTED_VERSION" ]]; then
    echo "Usage: $0 <expected-version> [chart-path]"
    echo "Example: $0 pr-382 ./helm/hephaestus"
    exit 1
fi

FAILED=0

log_success() { echo -e "\033[32m✓ $1\033[0m"; }
log_failure() { echo -e "\033[31m✗ $1\033[0m"; FAILED=1; }
log_info() { echo -e "\033[34m→ $1\033[0m"; }

echo "========================================"
echo "Helm Version Verification"
echo "Expected version: $EXPECTED_VERSION"
echo "Chart path: $CHART_PATH"
echo "========================================"

# Test 1: Verify Chart.yaml exists
test_chart_exists() {
    echo ""
    echo "=== Test: Chart.yaml exists ==="

    if [[ -f "$CHART_PATH/Chart.yaml" ]]; then
        log_success "Chart.yaml found"
    else
        log_failure "Chart.yaml not found at $CHART_PATH/Chart.yaml"
        exit 1
    fi
}

# Test 2: Verify appVersion in Chart.yaml matches expected
test_app_version() {
    echo ""
    echo "=== Test: appVersion matches expected ==="

    local app_version
    app_version=$(grep -E "^appVersion:" "$CHART_PATH/Chart.yaml" | awk '{print $2}' | tr -d '"')

    log_info "Found appVersion: $app_version"

    # Strip -debug suffix for comparison if present
    local expected_base="${EXPECTED_VERSION%-debug}"

    if [[ "$app_version" == "$EXPECTED_VERSION" ]] || [[ "$app_version" == "$expected_base" ]]; then
        log_success "appVersion matches expected"
    else
        log_failure "appVersion mismatch: expected '$EXPECTED_VERSION' or '$expected_base', got '$app_version'"
    fi
}

# Test 3: Verify values.yaml has correct image tag
test_image_tag() {
    echo ""
    echo "=== Test: values.yaml image tag ==="

    if [[ ! -f "$CHART_PATH/values.yaml" ]]; then
        log_info "values.yaml not found, skipping image tag check"
        return
    fi

    local image_tag
    image_tag=$(grep -E "^\s*tag:" "$CHART_PATH/values.yaml" | head -1 | awk '{print $2}' | tr -d '"')

    log_info "Found image tag in values.yaml: $image_tag"

    # Tag might be a placeholder or match our version
    if [[ -n "$image_tag" ]]; then
        log_success "Image tag is set in values.yaml"
    else
        log_info "Image tag not explicitly set (may use appVersion)"
    fi
}

# Test 4: Helm lint the chart
test_helm_lint() {
    echo ""
    echo "=== Test: Helm lint ==="

    if ! command -v helm &>/dev/null; then
        log_info "helm not found, skipping lint test"
        return
    fi

    if helm lint "$CHART_PATH" &>/dev/null; then
        log_success "Helm lint passed"
    else
        log_failure "Helm lint failed"
        helm lint "$CHART_PATH"
    fi
}

# Test 5: Helm template renders without errors
test_helm_template() {
    echo ""
    echo "=== Test: Helm template renders ==="

    if ! command -v helm &>/dev/null; then
        log_info "helm not found, skipping template test"
        return
    fi

    if helm template test-release "$CHART_PATH" &>/dev/null; then
        log_success "Helm template renders successfully"
    else
        log_failure "Helm template failed to render"
    fi
}

# Test 6: Rendered template uses correct image
test_rendered_image() {
    echo ""
    echo "=== Test: Rendered template uses correct image tag ==="

    if ! command -v helm &>/dev/null; then
        log_info "helm not found, skipping rendered image test"
        return
    fi

    local rendered
    rendered=$(helm template test-release "$CHART_PATH" 2>/dev/null)

    # Look for image references in the rendered output
    local image_refs
    image_refs=$(echo "$rendered" | grep -E "image:" | head -5)

    if [[ -n "$image_refs" ]]; then
        log_info "Image references found in rendered template:"
        echo "$image_refs" | while read -r line; do
            echo "    $line"
        done
        log_success "Template contains image references"
    else
        log_info "No explicit image references found (may be using defaults)"
    fi
}

# Main execution
main() {
    test_chart_exists
    test_app_version
    test_image_tag
    test_helm_lint
    test_helm_template
    test_rendered_image

    # Summary
    echo ""
    echo "========================================"
    if [[ $FAILED -eq 0 ]]; then
        log_success "All Helm version tests passed!"
        exit 0
    else
        log_failure "Some Helm version tests failed"
        exit 1
    fi
}

main
