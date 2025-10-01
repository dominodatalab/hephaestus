package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

const (
	// AccessLabel is kubernetes metadata set by clients required to allow reading secrets by Hephaestus.
	// Safeguards against accidental secret exposure / exfiltration.
	AccessLabel = "hephaestus-accessible"
	// OwnedLabel is kubernetes metadata set by clients, to manage secret lifetime.
	// When set, the given secret gets deleted at the same time as the ImageBuild that uses it.
	OwnedLabel = "hephaestus-owned"
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
	// NOTE: this field was previously used to assert the presence of an auth entry inside of secret credentials. if the
	//  Server was missing, then an error was raised. this design is limiting because it requires users to create
	//  several `registryAuth` items with the same secret if they want to verify the presence. in a future api version,
	//  we may remove the Server field from this type and replace it with one or more fields that service the needs all
	//  credential types.
	Server string `json:"server,omitempty"`

	// NOTE: this field was previously used to determine whether to fetch credentials from the cloud a given server.
	// this is now done automatically and this field is no longer necessary.
	CloudProvided *bool `json:"cloudProvided,omitempty"`

	BasicAuth *BasicAuthCredentials `json:"basicAuth,omitempty"`
	Secret    *SecretCredentials    `json:"secret,omitempty"`
}

type SecretReference struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// ImageBuildStatusTransitionMessage contains information about ImageBuild status transitions.
//
// This type is used to publish JSON-formatted messages to one or more configured messaging
// endpoints when ImageBuild resources undergo phase changes during the build process.
type ImageBuildStatusTransitionMessage struct {
	// Name of the ImageBuild resource that underwent a transition.
	Name string `json:"name"`
	// Annotations present on the resource.
	Annotations map[string]string `json:"annotations,omitempty"`
	// ObjectLink points to the resource inside the Kubernetes API.
	ObjectLink string `json:"objectLink"`
	// PreviousPhase of the resource.
	PreviousPhase Phase `json:"previousPhase"`
	// CurrentPhase of the resource.
	CurrentPhase Phase `json:"currentPhase"`
	// OccurredAt indicates when the transition occurred.
	OccurredAt metav1.Time `json:"occurredAt"`
	// ImageURLs contains a list of fully-qualified registry images.
	// This field is only populated when an ImageBuild transitions to PhaseSucceeded.
	ImageURLs []string `json:"imageURLs,omitempty"`
	// ErrorMessage contains the details of error when one occurs.
	ErrorMessage string `json:"errorMessage,omitempty"`
}
