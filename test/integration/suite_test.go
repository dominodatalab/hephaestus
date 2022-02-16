package integration

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

func TestIntegration(t *testing.T) {
	suite.Run(t, &TestSuite{})
}

type TestSuite struct {
	suite.Suite
}

func (s *TestSuite) SetupSuite() {
	// create k8s cluster
	// install docker registry
	// install rmq
}

func (s *TestSuite) TearDownSuite() {
	// clean up resources
}

/* Test Cases:

- test caching
- test building
	- no cache
	- with cache
	- with exports
	- no exports
	- build args
	- concurrent
	- multi-stage
	- multi-tag
- test messaging
- test istio
- test eks, aks, gke

*/
