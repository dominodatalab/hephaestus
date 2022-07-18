package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ImageCacheSpec struct {
	Images       []string              `json:"images"`
	RegistryAuth []RegistryCredentials `json:"registryAuth,omitempty"`
}

type ImageCacheStatus struct {
	BuildkitPods []string           `json:"buildkitPods,omitempty"`
	CachedImages []string           `json:"cachedImages,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
	Phase        Phase              `json:"phase,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ic
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Cached Images",type=string,JSONPath=".status.cachedImages"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Target Images",type=string,JSONPath=".spec.images",priority=10
// +kubebuilder:printcolumn:name="Target Pods",type=string,JSONPath=".status.buildkitPods",priority=10

type ImageCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageCacheSpec   `json:"spec,omitempty"`
	Status ImageCacheStatus `json:"status,omitempty"`
}

func (in *ImageCache) GetConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

func (in *ImageCache) GetPhase() Phase {
	return in.Status.Phase
}

func (in *ImageCache) SetPhase(p Phase) {
	in.Status.Phase = p
}

// +kubebuilder:object:root=true

type ImageCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageCache `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageCache{}, &ImageCacheList{})
}
