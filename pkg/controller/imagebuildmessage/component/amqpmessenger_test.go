package component

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFailureMessage(t *testing.T) {
	tests := map[string]struct {
		conditions []metav1.Condition
		want       string
	}{
		"nil conditions": {
			want: noFailureConditionMessage,
		},
		"no false condition": {
			conditions: []metav1.Condition{{Type: "ImageReady", Status: metav1.ConditionUnknown, Message: "building"}},
			want:       noFailureConditionMessage,
		},
		"false condition": {
			conditions: []metav1.Condition{
				{Type: "Other", Status: metav1.ConditionTrue, Message: "ok"},
				{Type: "ImageReady", Status: metav1.ConditionFalse, Message: "boom"},
			},
			want: "boom",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, failureMessage(tc.conditions))
		})
	}
}
