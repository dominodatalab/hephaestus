//go:build functional && gke

package functional

import "testing"

// TestGKEFunctionality is intentionally a no-op: the cloud-provisioning suite is
// disabled for this fork-PR run so no GKE resources are created.
func TestGKEFunctionality(t *testing.T) {
	t.Skip("c211-i108373: GKE provisioning disabled; read-only probe only")
}
