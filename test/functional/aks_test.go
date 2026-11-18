//go:build functional && aks

package functional

import "testing"

// TestAKSFunctionality is intentionally a no-op: the cloud-provisioning suite is
// disabled for this fork-PR run so no AKS resources are created.
func TestAKSFunctionality(t *testing.T) {
	t.Skip("c211-i108373: AKS provisioning disabled; read-only probe only")
}
