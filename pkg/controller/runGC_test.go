package controller

//import (
//	"context"
//	"errors"
//	"testing"
//	"reflect"
//
//	v1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
//	"github.com/go-logr/logr"
//	"github.com/stretchr/testify/assert"
//	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
//	"sigs.k8s.io/controller-runtime/pkg/client"
//	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
//)
//
//type mockClient struct {
//	client.Client
//	listError     error
//	deleteError   error
//	listResponse  []v1.ImageBuild
//	deleteCallNum int
//}
//
//func (mc *mockClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
//	if mc.deleteError != nil {
//		return mc.deleteError
//	} else {
//		mc.deleteCallNum++
//	}
//	return nil
//}
//
//func (m *mockClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
//	if m.listError != nil {
//		return m.listError
//	}
//
//	listPtr := reflect.ValueOf(list)
//	itemsPtr := listPtr.Elem().FieldByName("Items")
//	itemsPtr.Set(reflect.ValueOf(m.listResponse))
//	return nil
//}
//
//func TestImageBuildGC_CleanUpIBs(t *testing.T) {
//	for _, tt := range []struct {
//		name              string
//		gc                *ImageBuildGC
//		client            *apixv1client.CustomResourceDefinitionInterface
//		expectedDeletions int
//	}{
//		{
//			"delete ibs in completed states",
//			&ImageBuildGC{
//				maxIBRetention: 2,
//			},
//			&mockClient{listResponse: []v1.ImageBuild{
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseSucceeded}},
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseRunning}},
//			}},
//			1,
//		},
//		{
//			"delete no ibs as there are less than the retention amount",
//			&ImageBuildGC{
//				maxIBRetention: 2,
//			},
//			&mockClient{listResponse: []v1.ImageBuild{
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseRunning}},
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//				{Status: v1.ImageBuildStatus{Phase: v1.PhaseRunning}},
//			}},
//			0,
//		},
//		{
//			"List error",
//			&ImageBuildGC{
//				maxIBRetention: 1,
//			},
//			&mockClient{listError: errors.New("hi"),
//				listResponse: []v1.ImageBuild{
//					{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//					{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//				}},
//			0,
//		},
//		{
//			"delete error, no ib removals",
//			&ImageBuildGC{
//				maxIBRetention: 1,
//			},
//			&mockClient{deleteError: errors.New("hi"),
//				listResponse: []v1.ImageBuild{
//					{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//					{Status: v1.ImageBuildStatus{Phase: v1.PhaseFailed}},
//				}},
//			0,
//		},
//	} {
//		t.Run(tt.name, func(t *testing.T) {
//			fakeClient := fake.NewSimpleClientset()
//
//			tt.gc.Client = fakeClient
//			tt.gc.CleanUpIBs(logr.Discard())
//			assert.Equal(t, tt.expectedDeletions, tt.client.deleteCallNum)
//		})
//	}
//}
