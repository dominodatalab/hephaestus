package component

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

func TestRecordImageBuildPhase(t *testing.T) {
	imageBuildPhaseTotal.Reset()
	t.Cleanup(imageBuildPhaseTotal.Reset)

	// Two failures of the same reason, one of another, and one success.
	recordImageBuildPhase(hephv1.PhaseFailed, "CredentialsValidateError")
	recordImageBuildPhase(hephv1.PhaseFailed, "CredentialsValidateError")
	recordImageBuildPhase(hephv1.PhaseFailed, "ImageBuildError")
	recordImageBuildPhase(hephv1.PhaseSucceeded, "")

	expected := `
# HELP hephaestus_imagebuild_phase_total Count of ImageBuild terminal phase transitions, labeled by phase and failure reason.
# TYPE hephaestus_imagebuild_phase_total counter
hephaestus_imagebuild_phase_total{failure_reason="",phase="Succeeded"} 1
hephaestus_imagebuild_phase_total{failure_reason="CredentialsValidateError",phase="Failed"} 2
hephaestus_imagebuild_phase_total{failure_reason="ImageBuildError",phase="Failed"} 1
`

	if err := testutil.CollectAndCompare(imageBuildPhaseTotal, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metric state:\n%v", err)
	}
}
