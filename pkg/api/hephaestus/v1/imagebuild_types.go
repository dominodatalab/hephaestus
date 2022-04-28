package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dominodatalab/hephaestus/pkg/jsonpatch"
)

type ImageBuildAMQPOverrides struct {
	ExchangeName string `json:"exchangeName,omitempty"`
	QueueName    string `json:"queueName,omitempty"`
}

type ImageBuildSpec struct {
	Context       string                   `json:"context,omitempty"`
	Images        []string                 `json:"images,omitempty"`
	BuildArgs     []string                 `json:"buildArgs,omitempty"`
	LogKey        string                   `json:"logKey,omitempty"`
	RegistryAuth  []RegistryCredentials    `json:"registryAuth,omitempty"`
	AMQPOverrides *ImageBuildAMQPOverrides `json:"amqpOverrides,omitempty"`

	// TODO: implement the functionality for the following fields
	ImageSizeLimit          *int64 `json:"imageSizeLimit,omitempty"`
	DisableBuildCache       bool   `json:"disableBuildCache,omitempty"`
	DisableLayerCacheExport bool   `json:"disableLayerCacheExport,omitempty"`
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
	// +kubebuilder:default={phase: "Created", transitions: {{previousPhase: "", phase: "Created", processed: false}}}
	Status ImageBuildStatus `json:"status,omitempty"`
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
	ops := jsonpatch.Operations{
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

	patch, err := ops.MarshallJSON()
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
