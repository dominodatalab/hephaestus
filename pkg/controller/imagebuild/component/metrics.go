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

func init() {
	metrics.Registry.MustRegister(imageBuildPhaseTotal)
}

// recordImageBuildPhase increments the terminal-phase counter for the given phase and
// failure reason. failureReason should be empty for non-failure phases.
func recordImageBuildPhase(phase hephv1.Phase, failureReason string) {
	imageBuildPhaseTotal.WithLabelValues(string(phase), failureReason).Inc()
}
