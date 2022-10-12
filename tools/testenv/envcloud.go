package testenv

import (
	"context"
	"errors"
	"fmt"
	golog "log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/hc-install/src"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/helmfile/helmfile/pkg/app"
	"github.com/helmfile/helmfile/pkg/config"
	"github.com/helmfile/helmfile/pkg/helmexec"
)

const terraformVersion = "1.3.1"

var terraformInstallDir = filepath.Join(runtimePath, fmt.Sprintf("terraform-%s", terraformVersion))

type removableInstall interface {
	src.Installable
	src.Removable
}

// CloudConfig represents a type that contains the information required to build cloud-based Kubernetes cluster from one
// of the templates defined in the project resources/ directory.
type CloudConfig interface {
	Vars() []byte
	Validate() error
	ResourcePath() string
}

// CloudEnvManager is a Terraform-based Manager used to create Kubernetes clusters in the cloud.
type CloudEnvManager struct {
	log       *golog.Logger
	installer removableInstall
	terraform *tfexec.Terraform
}

// NewCloudEnvManager creates a CloudEnvManager with the resources necessary to invoke Terraform.
//
// The CloudConfig implementation will determine which cloud-provided cluster is created, such as GKEConfig.
// All Terraform operations will be streamed to stdout when verbose is set to true.
func NewCloudEnvManager(ctx context.Context, config CloudConfig, verbose bool) (*CloudEnvManager, error) {
	testenvLog.Println("processing configuration")
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config is invalid: %w", err)
	}

	testenvLog.Println("verifying terraform install")
	installer, execPath, err := verifyTerraformInstall(ctx, verbose)
	if err != nil {
		return nil, fmt.Errorf("failed to verify terraform install: %w", err)
	}

	testenvLog.Println("building terraform working directory")
	workingDir, err := buildWorkingDir(config)
	if err != nil {
		return nil, fmt.Errorf("failed to build terraform working dir: %w", err)
	}

	terraform, err := tfexec.NewTerraform(workingDir, execPath)
	if err != nil {
		return nil, fmt.Errorf("terraform client creation failed: %w", err)
	}
	if verbose {
		terraform.SetStdout(os.Stdout)
	}
	terraform.SetLogger(newLogger("tf-client"))

	testenvLog.Println("initializing terraform working directory")
	if err = terraform.Init(ctx); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w", err)
	}

	testenvLog.Println("successfully created manager")
	return &CloudEnvManager{
		log:       newLogger("cloud-env-manager"),
		installer: installer,
		terraform: terraform,
	}, nil
}

func (m *CloudEnvManager) Create(ctx context.Context) error {
	if err := m.terraform.Apply(ctx); err != nil {
		return fmt.Errorf("terraform apply failed: %w", err)
	}

	return nil
}

func (m *CloudEnvManager) HelmfileApply(ctx context.Context, helmfilePath string) error {
	kubeconfig, err := m.KubeconfigBytes(ctx)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp("", "testenv-kubeconfig-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())

	if _, err = f.Write(kubeconfig); err != nil {
		return err
	}

	if err = os.Setenv("KUBECONFIG", f.Name()); err != nil {
		return err
	}
	defer os.Unsetenv("KUBECONFIG")

	globalOpts := &config.GlobalOptions{File: helmfilePath}
	globalOpts.SetLogger(helmexec.NewLogger(os.Stdout, "info"))
	globalImpl := config.NewGlobalImpl(globalOpts)
	applyImpl := config.NewApplyImpl(globalImpl, &config.ApplyOptions{SkipDiffOnInstall: true})

	helmfile := app.New(applyImpl)
	if err = helmfile.Apply(applyImpl); err != nil {
		return err
	}

	return nil
}

func (m *CloudEnvManager) KubeconfigBytes(ctx context.Context) ([]byte, error) {
	output, err := m.terraform.Output(ctx)
	if err != nil {
		return nil, err
	}

	kubeconfig, ok := output["kubeconfig"]
	if !ok {
		return nil, errors.New("terraform output missing kubeconfig variable")
	}

	unescaped, err := strconv.Unquote(string(kubeconfig.Value))
	if err != nil {
		return nil, fmt.Errorf("failed to unescape kubeconfig: %w", err)
	}

	return []byte(unescaped), nil
}

func (m *CloudEnvManager) Destroy(ctx context.Context) error {
	// TODO: destroy helmfile apps to clean up garbage

	if err := m.terraform.Destroy(ctx); err != nil {
		return fmt.Errorf("terraform destroy failed: %w", err)
	}

	if err := m.installer.Remove(ctx); err != nil {
		return fmt.Errorf("terraform exec removal failed: %w", err)
	}

	return nil
}

func verifyTerraformInstall(ctx context.Context, verbose bool) (removableInstall, string, error) {
	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		Version:    version.Must(version.NewVersion(terraformVersion)),
		InstallDir: terraformInstallDir,
	}
	if verbose {
		installer.SetLogger(newLogger("tf-installer"))
	}

	execPath := filepath.Join(terraformInstallDir, "terraform")

	if _, err := os.Stat(execPath); os.IsNotExist(err) {
		if err = os.MkdirAll(terraformInstallDir, 0755); err != nil {
			return nil, "", err
		}

		if execPath, err = installer.Install(ctx); err != nil {
			return nil, "", fmt.Errorf("terraform install failed: %w", err)
		}
	}

	return installer, execPath, nil
}

func buildWorkingDir(config CloudConfig) (string, error) {
	workingDir := filepath.Join("testenv", config.ResourcePath())
	if err := os.MkdirAll(workingDir, 0755); err != nil {
		return "", err
	}

	entries, err := resources.ReadDir(config.ResourcePath())
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		fp := filepath.Join(config.ResourcePath(), entry.Name())
		content, err := resources.ReadFile(fp)
		if err != nil {
			return "", err
		}

		target := filepath.Join(workingDir, entry.Name())
		if err = os.WriteFile(target, content, 0644); err != nil {
			return "", err
		}
	}

	if err = os.WriteFile(filepath.Join(workingDir, "terraform.tfvars"), config.Vars(), 0644); err != nil {
		return "", err
	}

	return workingDir, nil
}
