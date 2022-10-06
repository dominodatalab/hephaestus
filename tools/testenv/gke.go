package testenv

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-exec/tfexec"
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
func (c GKEConfig) Vars() []*tfexec.VarOption {
	opts := []*tfexec.VarOption{
		tfexec.Var(fmt.Sprintf("project_id=%s", c.ProjectID)),
		tfexec.Var(fmt.Sprintf("region=%s", c.Region)),
	}

	if strings.TrimSpace(c.KubernetesVersion) != "" {
		opts = append(opts, tfexec.Var(fmt.Sprintf("kubernetes_version=%s", c.KubernetesVersion)))
	}

	return opts
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
