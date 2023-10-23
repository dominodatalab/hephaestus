package controller

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

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
	cmd.PersistentFlags().StringVarP(&cfgFile, "config", "c",
		"hephaestus.yaml", "configuration file")
	cmd.PersistentFlags().StringVarP(&config.CompressionMethod,
		"compression", "d", "gzip", "Compression method options: zstd,estargz")
	cmd.AddCommand(
		newStartCommand(),
		newCRDApplyCommand(),
		newCRDDeleteCommand(),
	)

	return cmd
}

func newStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config.CompressionMethod, _ = cmd.Flags().GetString("compression")
			fmt.Printf("BuildKit compression method: %s enabled\n", config.CompressionMethod)
			cfgFile, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}

			cfg, err := config.LoadFromFile(cfgFile)
			if err != nil {
				return err
			}

			if err = cfg.Validate(); err != nil {
				return err
			}

			return controller.Start(cfg)
		},
	}
}

func newCRDApplyCommand() *cobra.Command {
	var istioEnabled bool
	cmd := &cobra.Command{
		Use:   "crd-apply",
		Short: "Apply custom resource definitions to a cluster",
		Long: `Apply all "hephaestus.dominodatalab.com" CRDs to a cluster.

Apply Rules:
  - When a definition is missing, it will be created
  - If a definition is already present, then it will be updated
  - Updating definitions that have not changed results in a no-op`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return crd.Apply(context.Background(), istioEnabled)
		},
	}
	cmd.PersistentFlags().BoolVar(&istioEnabled, "istio-enabled", false, "Enable support for Istio sidecar container")

	return cmd
}

func newCRDDeleteCommand() *cobra.Command {
	var istioEnabled bool
	cmd := &cobra.Command{
		Use:   "crd-delete",
		Short: "Delete custom resource definitions from a cluster",
		Long: `Delete all "hephaestus.dominodatalab.com" CRDs from a cluster.

Any running builds will be decommissioned when this operation runs. This will
only attempt to remove definitions that are already present in Kubernetes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return crd.Delete(context.Background(), istioEnabled)
		},
	}
	cmd.PersistentFlags().BoolVar(&istioEnabled, "istio-enabled", false, "Enable support for Istio sidecar container")

	return cmd
}
