package main

import (
	"github.com/dominodatalab/hephaestus/pkg/cmd"
	"github.com/dominodatalab/hephaestus/pkg/cmd/controller"
)

func main() {
	if err := controller.NewCommand().Execute(); err != nil {
		cmd.ExitWithErr(err)
	}
}
