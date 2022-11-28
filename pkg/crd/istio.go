package crd

import (
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

const (
	checkURL  = "http://localhost:15021/healthz/ready"
	finishURL = "http://localhost:15020/quitquitquit"
)

type headPostClient interface {
	Head(url string) (*http.Response, error)
	Post(url string, bodyType string, body interface{}) (*http.Response, error)
}

var retryClient headPostClient

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
	client := retryablehttp.NewClient()
	client.RetryMax = 10
	client.RetryWaitMin = 1 * time.Second
	client.RetryWaitMax = 1 * time.Second

	retryClient = client
}
