package v1

const (
	ImageBuildKind = "ImageBuild"
	ImageCacheKind = "ImageCache"
)

const (
	CacheModeMax = "max"
	CacheModeMin = "min"
)

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

type BasicAuthCredentials struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type SecretCredentials struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type RegistryCredentials struct {
	Server string `json:"server,omitempty"`

	CloudProvided *bool                 `json:"cloudProvided,omitempty"`
	BasicAuth     *BasicAuthCredentials `json:"basicAuth,omitempty"`
	Secret        *SecretCredentials    `json:"secret,omitempty"`
}
