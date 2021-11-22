package main

import (
	"fmt"
	"os"

	"github.com/dominodatalab/hephaestus/pkg/cmd/controller"
)

func main() {
	if err := controller.NewCommand().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
