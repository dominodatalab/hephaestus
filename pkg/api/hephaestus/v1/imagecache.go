package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ImageCacheSpec struct {
	Images       []string              `json:"images"`
	RegistryAuth []RegistryCredentials `json:"registryAuth,omitempty"`
}

type ImageCacheStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Phase      string             `json:"phase,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=imgc
// +kubebuilder:subresource:status

type ImageCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageCacheSpec   `json:"spec,omitempty"`
	Status ImageCacheStatus `json:"status,omitempty"`
}

func (in *ImageCache) GetConditions() *[]metav1.Condition {
	return &in.Status.Conditions
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
