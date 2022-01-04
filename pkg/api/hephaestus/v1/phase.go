package v1

// Phase represents a step in a resource processing lifecycle.
type Phase string

const (
	// PhaseInitializing indicates that an execution sequence is preparing to run.
	PhaseInitializing Phase = "Initializing"
	// PhaseRunning indicates that an execution sequence has begun.
	PhaseRunning Phase = "Running"
	// PhaseSucceeded indicates that an execution sequence successfully operated against a resource.
	PhaseSucceeded Phase = "Succeeded"
	// PhaseFailed indicates an error was encountered during execution.
	PhaseFailed Phase = "Failed"
)
