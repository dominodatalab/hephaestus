package kubernetes

import (
	"time"

	"github.com/go-logr/logr"
	"github.com/hashicorp/go-retryablehttp"
)

const (
	checkURL  = "http://localhost:15021/healthz/ready"
	finishURL = "http://localhost:15020/quitquitquit"
)

var retryClient *retryablehttp.Client

func WaitForIstioSidecar(logger logr.Logger) (func(), error) {
	logger.Info("Checking istio sidecar")
	resp, err := retryClient.Head(checkURL)
	if err != nil {
		logger.Error(err, "Istio sidecar is not ready")
		return nil, err
	}
	defer resp.Body.Close()

	logger.Info("Istio sidecar available")
	fn := func() {
		logger.Info("Triggering istio termination")
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
