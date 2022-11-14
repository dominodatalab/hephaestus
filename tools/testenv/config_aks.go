package testenv

import (
	"errors"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// AKSConfig is used to create AKS test environments.
type AKSConfig struct {
	// Location in Azure where cluster will be created.
	Location string
	// KubernetesVersion supported by AKS.
	KubernetesVersion string
}

// ResourcePath to Terraform module.
func (c AKSConfig) ResourcePath() string {
	return "resources/terraform/azure-aks"
}

// Vars derived from struct fields.
func (c AKSConfig) Vars() []byte {
	f := hclwrite.NewEmptyFile()

	root := f.Body()
	root.SetAttributeValue("location", cty.StringVal(c.Location))

	if strings.TrimSpace(c.KubernetesVersion) != "" {
		root.SetAttributeValue("kubernetes_version", cty.StringVal(c.KubernetesVersion))
	}

	return f.Bytes()
}

// Validate ensures required variables are provided.
func (c AKSConfig) Validate() (err error) {
	if strings.TrimSpace(c.Location) == "" {
		err = errors.New("azure location cannot be blank")
	}

	return err
}
