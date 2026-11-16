package phase

import (
	"context"
	"errors"
	"testing"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

func TestSetFailedPersistsConditionWithPhase(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, hephv1.AddToScheme(scheme))

	ib := &hephv1.ImageBuild{ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ib).WithStatusSubresource(ib).Build()

	ctx := &core.Context{
		Context:    context.Background(),
		Log:        logr.Discard(),
		Client:     c,
		Conditions: core.NewConditionHelper(ib),
	}
	h := &TransitionHelper{Client: c, ReadyCondition: "ImageReady"}

	require.NoError(t, h.SetFailed(ctx, ib, errors.New("boom")))

	var got hephv1.ImageBuild
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ib), &got))
	assert.Equal(t, hephv1.PhaseFailed, got.Status.Phase)
	require.Len(t, got.Status.Conditions, 1)
	assert.Equal(t, "ImageReady", got.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, got.Status.Conditions[0].Status)
	assert.Equal(t, "boom", got.Status.Conditions[0].Message)
}
