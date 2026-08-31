package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ImageBuildAMQPOverrides struct {
	ExchangeName string `json:"exchangeName,omitempty"`
	QueueName    string `json:"queueName,omitempty"`
}

// ImageBuildSpec specifies the desired state of an ImageBuild resource.
type ImageBuildSpec struct {
	// Context is a remote URL used to fetch the build context.  Overrides dockerfileContents if present.
	Context string `json:"context,omitempty"`
	// DockerfileContents specifies the contents of the Dockerfile directly in the CR.  Ignored if context is present.
	DockerfileContents string `json:"dockerfileContents,omitempty"`
	// Images is a list of images to build and push.
	Images []string `json:"images,omitempty"`
	// BuildArgs are applied to the build at runtime.
	BuildArgs []string `json:"buildArgs,omitempty"`
	// LogKey is used to uniquely annotate build logs for post-processing
	LogKey string `json:"logKey,omitempty"`
	// RegistryAuth credentials used to pull/push images from/to private registries.
	RegistryAuth []RegistryCredentials `json:"registryAuth,omitempty"`
	// AMQPOverrides to the main controller configuration.
	AMQPOverrides *ImageBuildAMQPOverrides `json:"amqpOverrides,omitempty"`
	// ImportRemoteBuildCache from one or more canonical image references when building the images.
	ImportRemoteBuildCache []string `json:"importRemoteBuildCache,omitempty"`
	// DisableLocalBuildCache  will disable the use of the local cache when building the images.
	DisableLocalBuildCache bool `json:"disableBuildCache,omitempty"`
	// DisableCacheLayerExport will remove the "inline" cache metadata from the image configuration.
	DisableCacheLayerExport bool `json:"disableCacheExport,omitempty"`
	// Secrets provides references to Kubernetes secrets to expose to individual image builds.
	Secrets []SecretReference `json:"secrets,omitempty"`
	// Platforms specifies one or more target OS/architecture combinations to build for, using
	// "os/arch[/variant]" syntax (e.g. "linux/amd64", "linux/arm64"). Each requested platform must be
	// served by a configured buildkit pool, natively or via emulation, or the request is rejected at
	// admission time. When empty, the build runs on whatever platform the leased worker natively runs.
	Platforms []string `json:"platforms,omitempty"`
}

type ImageBuildTransition struct {
	PreviousPhase Phase `json:"previousPhase"`
	Phase         Phase `json:"phase"`
	// +optional
	OccurredAt metav1.Time `json:"occurredAt,omitzero"`
}

type ImageBuildStatus struct {
	// AllocationTime is the total time spent allocating a build pod.
	AllocationTime string `json:"allocationTime,omitempty"`
	// BuildTime is the total time spent during the image build process.
	BuildTime string `json:"buildTime,omitempty"`
	// BuilderAddr is the routable address to the buildkit pod used during the image build process.
	BuilderAddr string `json:"builderAddr,omitempty"`
	// CompressedImageSizeBytes is the total size of all the compressed layers in the image.
	// For a build spanning more than one requested platform, see Platforms instead.
	CompressedImageSizeBytes string `json:"compressedImageSizeBytes,omitempty"`
	// Digest is the image digest. For a multi-platform build this is the manifest list/index digest.
	Digest string `json:"digest,omitempty"`
	// Map of string keys and values corresponding OCI image config labels.
	// Labels contains arbitrary metadata for the container.
	// For a build spanning more than one requested platform, see Platforms instead.
	Labels map[string]string `json:"labels,omitempty"`
	// Platforms contains a per-platform breakdown, populated whenever Spec.Platforms requests more
	// than one platform - whether they were built by a single buildkit pool in one multi-platform
	// solve, or fanned out across more than one pool.
	Platforms []PlatformResult `json:"platforms,omitempty"`

	Conditions  []metav1.Condition     `json:"conditions,omitempty"`
	Transitions []ImageBuildTransition `json:"transitions,omitempty"`
	Phase       Phase                  `json:"phase,omitempty"`

	unappliedTransition ImageBuildTransition `json:"-"`
}

// PlatformResult records the outcome of building a single platform as part of a build spanning more
// than one requested platform, whether built by one buildkit pool in a single multi-platform solve or
// fanned out across more than one pool. Error is only ever set in the latter case, since a
// single-pool multi-platform solve either produces every platform or fails the build entirely.
type PlatformResult struct {
	// Platform is the "os/arch[/variant]" this result corresponds to.
	Platform string `json:"platform"`
	// Digest is the pushed single-platform image digest. Empty if Error is set.
	Digest string `json:"digest,omitempty"`
	// CompressedImageSizeBytes is the total size of all the compressed layers for this platform.
	CompressedImageSizeBytes string `json:"compressedImageSizeBytes,omitempty"`
	// Labels contains arbitrary OCI image config metadata for this platform.
	Labels map[string]string `json:"labels,omitempty"`
	// Error contains the failure reason when this platform failed to build. Only ever set for a
	// build fanned out across more than one buildkit pool.
	Error string `json:"error,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ib
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Allocation Time",type=string,JSONPath=".status.allocationTime"
// +kubebuilder:printcolumn:name="Build Time",type=string,JSONPath=".status.buildTime"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Builder Address",type=string,JSONPath=".status.builderAddr",priority=10

type ImageBuild struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec ImageBuildSpec `json:"spec,omitzero"`
	// +optional
	Status ImageBuildStatus `json:"status,omitzero"`
}

func (in *ImageBuild) ObjectKey() client.ObjectKey {
	return client.ObjectKey{Name: in.Name, Namespace: in.Namespace}
}

func (in *ImageBuild) GetConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

func (in *ImageBuild) GetPhase() Phase {
	return in.Status.Phase
}

func (in *ImageBuild) SetPhase(p Phase) {
	ibt := ImageBuildTransition{
		PreviousPhase: in.Status.Phase,
		Phase:         p,
		OccurredAt:    metav1.Time{Time: time.Now()},
	}

	in.Status.unappliedTransition = ibt
	in.Status.Transitions = append(in.Status.Transitions, ibt)
	in.Status.Phase = p
}

// +kubebuilder:object:root=true

type ImageBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []ImageBuild `json:"items"`
}
