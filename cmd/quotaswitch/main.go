package main

import (
	"fmt"
	"os"

	"github.com/lifan-builds/quotaswitch/internal/cli"
)

func main() {
	app, err := cli.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "quotaswitch: %v\n", err)
		os.Exit(1)
	}
	os.Exit(app.Run(os.Args[1:]))
}
