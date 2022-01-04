package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ImageBuildSpec struct {
	Context             string                `json:"context,omitempty"`
	Images              []string              `json:"images,omitempty"`
	BuildArgs           []string              `json:"buildArgs,omitempty"`
	CacheTag            string                `json:"cacheTag,omitempty"`
	CacheMode           string                `json:"cacheMode,omitempty"`
	DisableCacheExports bool                  `json:"disableCacheExports,omitempty"`
	DisableCacheImports bool                  `json:"disableCacheImports,omitempty"`
	RegistryAuth        []RegistryCredentials `json:"registryAuth,omitempty"`
}

type ImageBuildStatus struct {
	// BuildTime is the total time spend during the build process.
	BuildTime  string             `json:"buildTime,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Phase      Phase              `json:"phase,omitempty"`
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

	Spec   ImageBuildSpec   `json:"spec,omitempty"`
	Status ImageBuildStatus `json:"status,omitempty"`
}

func (in *ImageBuild) GetConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

func (in *ImageBuild) GetPhase() Phase {
	return in.Status.Phase
}

func (in *ImageBuild) SetPhase(p Phase) {
	in.Status.Phase = p
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
