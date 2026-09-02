package component

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

// imageBuildPhaseTotal counts ImageBuild terminal phase transitions, labeled by the
// terminal phase and (on failure) the failure reason. The failure_reason label uses the
// same vocabulary as the New Relic error.class attached at each failure site, so the
// metric can be correlated with APM TransactionError facets. failure_reason is empty on
// success.
var imageBuildPhaseTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "hephaestus_imagebuild_phase_total",
		Help: "Count of ImageBuild terminal phase transitions, labeled by phase and failure reason.",
	},
	[]string{"phase", "failure_reason"},
)

// imageBuildPlatformPhaseTotal counts terminal phase transitions broken down by requested platform,
// for builds that fan out across more than one buildkit pool (see Status.Platforms). It's additive
// to imageBuildPhaseTotal, not a replacement: single-pool builds never populate Status.Platforms, so
// they never increment this counter, leaving its cardinality bounded to fan-out builds only. Like
// imageBuildPhaseTotal, it's incremented only after the overall terminal phase is durably persisted.
var imageBuildPlatformPhaseTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "hephaestus_imagebuild_platform_phase_total",
		Help: "Count of terminal phase transitions for multi-pool ImageBuilds, " +
			"labeled by platform, phase, and failure reason.",
	},
	[]string{"platform", "phase", "failure_reason"},
)

func init() {
	metrics.Registry.MustRegister(imageBuildPhaseTotal)
	metrics.Registry.MustRegister(imageBuildPlatformPhaseTotal)
}

// recordImageBuildPhase increments the terminal-phase counter for the given phase and
// failure reason. failureReason should be empty for non-failure phases.
func recordImageBuildPhase(phase hephv1.Phase, failureReason string) {
	imageBuildPhaseTotal.WithLabelValues(string(phase), failureReason).Inc()
}

// recordImageBuildPlatformPhases increments imageBuildPlatformPhaseTotal for every platform in
// platforms, using the overall terminal phase/reason unless a platform's own result says otherwise:
// a platform with no recorded Error still succeeded even when the ImageBuild as a whole later failed
// (e.g. during manifest list assembly), so it's counted as Succeeded rather than inheriting the
// overall Failed outcome. A nil/empty platforms (the single-pool case) is a no-op.
func recordImageBuildPlatformPhases(platforms []hephv1.PlatformResult, phase hephv1.Phase, failureReason string) {
	for _, p := range platforms {
		platformPhase, platformReason := phase, failureReason
		if phase == hephv1.PhaseFailed && p.Error == "" {
			platformPhase, platformReason = hephv1.PhaseSucceeded, ""
		}
		imageBuildPlatformPhaseTotal.WithLabelValues(p.Platform, string(platformPhase), platformReason).Inc()
	}
}
