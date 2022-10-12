//go:build functional && aks
// +build functional,aks

package functional

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

func TestAKSFunctionality(t *testing.T) {
	suite.Run(t, new(AKSTestSuite))
}

type AKSTestSuite struct {
	suite.Suite
}

func (suite *AKSTestSuite) SetupSuite() {}

func (suite *AKSTestSuite) TearDownSuite() {}
