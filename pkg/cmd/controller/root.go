package controller

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller"
	"github.com/dominodatalab/hephaestus/pkg/crd"
)

func NewCommand() *cobra.Command {
	var cfgFile string

	cmd := &cobra.Command{
		Use:   "hephaestus-controller",
		Short: "OCI image build controller using buildkit",
	}
	cmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "hephaestus.yaml", "configuration file")
	cmd.AddCommand(
		newInitCommand(),
		newStartCommand(),
		newCRDApplyCommand(),
		newCRDDeleteCommand(),
	)

	return cmd
}

func newInitCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate config skeleton",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.GenerateDefaults()
			fmt.Println(cfg)

			bs, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}

			cfgFile, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}

			return os.WriteFile(cfgFile, bs, 0644)
		},
	}

	return cmd
}

func newStartCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgFile, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}

			cfg, err := config.LoadFromFile(cfgFile)
			if err != nil {
				return err
			}

			return controller.Start(cfg)
		},
	}

	return cmd
}

func newCRDApplyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "crd-apply",
		Short: "Apply custom resource definitions to a cluster",
		Long: `Apply all "hephaestus.dominodatalab.com" CRDs to a cluster.

Apply Rules:
  - When a definition is is missing, it will be created
  - If a definition is already present, then it will be updated
  - Updating definitions that have not changed results in a no-op`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return crd.Apply(context.Background())
		},
	}
}

func newCRDDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "crd-delete",
		Short: "Delete custom resource definitions from a cluster",
		Long: `Delete all "hephaestus.dominodatalab.com" CRDs from a cluster.

Any running builds will be decommissioned when this operation runs. This will
only attempt to remove definitions that are already present in Kubernetes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return crd.Delete(context.Background())
		},
	}
}
