package kubernetes

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RestConfig returns the canonical kubernetes REST config.
//
// Out-of-cluster loading is attempted first, followed by in-cluster when that fails.
func RestConfig() (*rest.Config, error) {
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	if cfg, err := kubeconfig.ClientConfig(); err == nil {
		return cfg, nil
	}

	return rest.InClusterConfig()
}

// Clientset creates and returns the canonical kubernetes clientset.
//
// If conf is nil, then RestConfig() is used to generate the config object.
func Clientset(conf *rest.Config) (kubernetes.Interface, error) {
	var err error

	if conf == nil {
		conf, err = RestConfig()
		if err != nil {
			return nil, err
		}
	}

	return kubernetes.NewForConfig(conf)
}
