//go:build integration

package integration

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

// TestStartServesWebhook checks that the running controller manager actually
// serves its admission webhook.
func TestStartServesWebhook(t *testing.T) {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ib := &hephv1.ImageBuild{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "webhook-probe-", Namespace: "default"},
			Spec: hephv1.ImageBuildSpec{
				Context: "https://example.com/context.tgz",
				Images:  []string{"example.com/foo:bar"},
			},
		}
		lastErr = k8sClient.Create(ctx, ib)
		if lastErr == nil {
			t.Log("webhook server is serving: API server admitted the ImageBuild create")
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("ImageBuild create never admitted within timeout; webhook server not serving. last error: %v", lastErr)
}
