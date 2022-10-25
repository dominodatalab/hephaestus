package testenv

import (
	"errors"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// EKSConfig is used to create EKS test environments.
type EKSConfig struct {
	Region            string
	KubernetesVersion string
}

func (e EKSConfig) Vars() []byte {
	f := hclwrite.NewEmptyFile()

	root := f.Body()
	root.SetAttributeValue("region", cty.StringVal(e.Region))

	if strings.TrimSpace(e.KubernetesVersion) != "" {
		root.SetAttributeValue("kubernetes_version", cty.StringVal(e.KubernetesVersion))
	}

	return f.Bytes()
}

func (e EKSConfig) Validate() (err error) {
	if strings.TrimSpace(e.Region) == "" {
		err = multierror.Append(err, errors.New("EKS region cannot be blank"))
	}

	return err
}

func (e EKSConfig) ResourcePath() string {
	return "resources/terraform/aws-eks"
}
