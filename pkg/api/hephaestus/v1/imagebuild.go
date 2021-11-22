package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ImageBuildSpec struct {
	Images              []string              `json:"images,omitempty"`
	Context             string                `json:"context,omitempty"`
	BuildArgs           []string              `json:"buildArgs,omitempty"`
	DisableCacheExports bool                  `json:"DisableCacheExports,omitempty"`
	DisableCacheImports bool                  `json:"DisableCacheImports,omitempty"`
	RegistryAuth        []RegistryCredentials `json:"registryAuth,omitempty"`
}

type ImageBuildStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Phase      string             `json:"phase,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=imgb
// +kubebuilder:subresource:status

type ImageBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageBuildSpec   `json:"spec,omitempty"`
	Status ImageBuildStatus `json:"status,omitempty"`
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
