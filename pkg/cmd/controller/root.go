package controller

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller"
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
