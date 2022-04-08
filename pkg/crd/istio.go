package crd

import (
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

const (
	checkURL  = "http://localhost:15021/healthz/ready"
	finishURL = "http://localhost:15020/quitquitquit"
)

var retryClient *retryablehttp.Client

func waitForIstioSidecar() (func(), error) {
	log.Info("Checking istio sidecar")
	resp, err := retryClient.Head(checkURL)
	if err != nil {
		log.Error(err, "Istio sidecar is not ready")
		return nil, err
	}
	defer resp.Body.Close()

	log.Info("Istio sidecar available")
	fn := func() {
		log.Info("Triggering istio termination")
		_, _ = retryClient.Post(finishURL, "", nil)
	}

	return fn, err
}

func init() {
	retryClient = retryablehttp.NewClient()
	retryClient.RetryMax = 10
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 1 * time.Second
}
