package component

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

func TestRunningAge_NoTransitions(t *testing.T) {
	ib := &hephv1.ImageBuild{}
	if got := runningAge(ib); got != "unknown" {
		t.Fatalf("expected unknown, got %q", got)
	}
}

func TestRunningAge_OnlySucceededTransition(t *testing.T) {
	ib := &hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Transitions: []hephv1.ImageBuildTransition{
				{Phase: hephv1.PhaseSucceeded, OccurredAt: metav1.Time{Time: time.Now().Add(-time.Hour)}},
			},
		},
	}
	if got := runningAge(ib); got != "unknown" {
		t.Fatalf("expected unknown (no Running/Initializing transition), got %q", got)
	}
}

func TestRunningAge_LatestRunningUsed(t *testing.T) {
	now := time.Now()
	ib := &hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Transitions: []hephv1.ImageBuildTransition{
				{Phase: hephv1.PhaseInitializing, OccurredAt: metav1.Time{Time: now.Add(-10 * time.Minute)}},
				{Phase: hephv1.PhaseRunning, OccurredAt: metav1.Time{Time: now.Add(-5 * time.Minute)}},
				{Phase: hephv1.PhaseFailed, OccurredAt: metav1.Time{Time: now.Add(-time.Minute)}},
			},
		},
	}
	got := runningAge(ib)
	if got == "unknown" {
		t.Fatalf("expected a duration, got unknown")
	}
	d, err := time.ParseDuration(got)
	if err != nil {
		t.Fatalf("result %q is not a duration: %v", got, err)
	}
	// should be ~5m, bounded loosely to tolerate test scheduling jitter
	if d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("expected ~5m, got %s", d)
	}
}
