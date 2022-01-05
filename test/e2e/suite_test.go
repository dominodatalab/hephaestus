package e2e

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

func TestE2E(t *testing.T) {
	suite.Run(t, &E2ETestSuite{})
}

type E2ETestSuite struct {
	suite.Suite
}

func (s *E2ETestSuite) SetupSuite() {
	// create k8s cluster
	// install docker registry
	// install rmq
}

func (s *E2ETestSuite) TearDownSuite() {
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

*/
