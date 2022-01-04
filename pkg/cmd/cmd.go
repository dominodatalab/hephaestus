package cmd

import (
	"fmt"
	"os"
)

func ExitWithErr(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
