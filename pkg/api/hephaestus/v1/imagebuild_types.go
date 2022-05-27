package v1

import (
	"encoding/json"
	"time"

	"gomodules.xyz/jsonpatch/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ImageBuildAMQPOverrides struct {
	ExchangeName string `json:"exchangeName,omitempty"`
	QueueName    string `json:"queueName,omitempty"`
}

// ImageBuildSpec specifies the desired state of an ImageBuild resource.
type ImageBuildSpec struct {
	// Context is a remote URL used to fetch the build context.
	Context string `json:"context,omitempty"`
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
}

type ImageBuildTransition struct {
	PreviousPhase Phase        `json:"previousPhase"`
	Phase         Phase        `json:"phase"`
	OccurredAt    *metav1.Time `json:"occurredAt,omitempty"`
	Processed     bool         `json:"processed"`
}

type ImageBuildStatus struct {
	// BuildTime is the total time spend during the build process.
	BuildTime   string                 `json:"buildTime,omitempty"`
	Conditions  []metav1.Condition     `json:"conditions,omitempty"`
	Transitions []ImageBuildTransition `json:"transitions,omitempty"`
	Phase       Phase                  `json:"phase,omitempty"`

	unappliedTransition ImageBuildTransition `json:"-"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ib
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Build Time",type=string,JSONPath=".status.buildTime"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

type ImageBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ImageBuildSpec `json:"spec,omitempty"`
	// +kubebuilder:default={phase: "Created", transitions: {{previousPhase: "", phase: "Created", processed: true}}}
	Status ImageBuildStatus `json:"status,omitempty"`
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
		OccurredAt:    &metav1.Time{Time: time.Now()},
		Processed:     false,
	}

	in.Status.unappliedTransition = ibt
	in.Status.Transitions = append(in.Status.Transitions, ibt)
	in.Status.Phase = p
}

func (in *ImageBuild) GetPatch() client.Patch {
	ops := []jsonpatch.Operation{
		{
			Operation: "replace",
			Path:      "/status/phase",
			Value:     in.Status.Phase,
		},
		{
			Operation: "add",
			Path:      "/status/transitions/-",
			Value:     in.Status.unappliedTransition,
		},
	}

	if in.Status.BuildTime != "" {
		ops = append(ops, jsonpatch.Operation{
			Operation: "add",
			Path:      "/status/buildTime",
			Value:     in.Status.BuildTime,
		})
	}

	patch, err := json.Marshal(ops)
	if err != nil {
		panic(err)
	}

	return client.RawPatch(types.JSONPatchType, patch)
}

// +kubebuilder:object:root=true

type ImageBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageBuild{}, &ImageBuildList{})
}
