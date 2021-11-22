package controller

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"sigs.k8s.io/yaml"

	"github.com/dominodatalab/hephaestus/pkg/controller"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
)

func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hephaestus-controller",
		Short: "OCI image build controller using buildkit",
	}
	cmd.AddCommand(
		newInitCommand(),
		newStartCommand(),
	)

	return cmd
}

func newInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return GenerateConfig()
		},
	}
}

func newStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start controller",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}

			return controller.Start(*cfg)
		},
	}
}

func InitConfig() *viper.Viper {
	v := viper.New()
	v.SetConfigName("hephaestus")
	v.AddConfigPath(".")
	v.SetTypeByDefaultValue(true)

	v.SetDefault("manager.metricsAddr", ":8080")
	v.SetDefault("manager.probeAddr", ":8081")
	v.SetDefault("manager.webhookPort", 9443)
	v.SetDefault("manager.enableLeaderElection", false)

	v.SetDefault("buildkit.portName", "daemon")
	v.SetDefault("buildkit.dynamicEndpoints", config.DynamicEndpointConfig{
		Name:      "buildkitd",
		Namespace: "hephaestus",
	})

	v.SetDefault("logging.development", true)

	return v
}

func GenerateConfig() error {
	v := InitConfig()
	conf := &config.Config{}

	if err := v.Unmarshal(conf); err != nil {
		return err
	}
	out, err := yaml.Marshal(conf)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("%s.yaml", "hephaestus")
	log.Printf("Writing %s", name)

	return os.WriteFile(name, out, 0644)
}

func LoadConfig() (*config.Config, error) {
	v := InitConfig()
	conf := &config.Config{}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}
	if err := v.Unmarshal(conf); err != nil {
		return nil, err
	}

	return conf, nil
}
