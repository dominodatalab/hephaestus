package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ImageBuildMessageAMQPConnection struct {
	URI      string `json:"uri"`
	Queue    string `json:"queue"`
	Exchange string `json:"exchange"`
}

type ImageBuildMessageSpec struct {
	AMQP ImageBuildMessageAMQPConnection `json:"amqp"`
}

type ImageBuildMessageRecord struct {
	SentAt  metav1.Time                       `json:"sentAt"`
	Message ImageBuildStatusTransitionMessage `json:"message"`
}

type ImageBuildMessageStatus struct {
	AMQPSentMessages []ImageBuildMessageRecord `json:"amqpSentMessages,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ibm
// +kubebuilder:subresource:status

type ImageBuildMessage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageBuildMessageSpec   `json:"spec,omitempty"`
	Status ImageBuildMessageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ImageBuildMessageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageBuildMessage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageBuildMessage{}, &ImageBuildMessageList{})
}
