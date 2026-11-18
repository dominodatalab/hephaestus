//go:build functional && eks

package functional

import "testing"

// TestEKSFunctionality is intentionally a no-op: the cloud-provisioning suite is
// disabled for this fork-PR run so no EKS resources are created.
func TestEKSFunctionality(t *testing.T) {
	t.Skip("c211-i108373: EKS provisioning disabled; read-only probe only")
}
