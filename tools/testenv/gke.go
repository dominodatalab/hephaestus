package testenv

import (
	"errors"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// GKEConfig is used to create GKE test environments.
type GKEConfig struct {
	// KubernetesVersion supported by GKE.
	KubernetesVersion string
	// ProjectID in GCP where cluster will be created.
	ProjectID string
	// Region in GCP where cluster will be created.
	Region string
}

// ResourcePath to Terraform module.
func (c GKEConfig) ResourcePath() string {
	return "resources/terraform/gcp-gke"
}

// Vars derived from struct fields.
func (c GKEConfig) Vars() []byte {
	f := hclwrite.NewEmptyFile()

	root := f.Body()
	root.SetAttributeValue("project_id", cty.StringVal(c.ProjectID))
	root.SetAttributeValue("region", cty.StringVal(c.Region))

	if strings.TrimSpace(c.KubernetesVersion) != "" {
		root.SetAttributeValue("kubernetes_version", cty.StringVal(c.KubernetesVersion))
	}

	return f.Bytes()
}

// Validate ensures required variables are provided.
func (c GKEConfig) Validate() (err error) {
	if strings.TrimSpace(c.ProjectID) == "" {
		err = multierror.Append(err, errors.New("GCP project id cannot be blank"))
	}

	if strings.TrimSpace(c.Region) == "" {
		err = multierror.Append(err, errors.New("GCP region cannot be blank"))
	}

	return err
}
