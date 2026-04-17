package main

import (
	"os"

	"github.com/selfbase-dev/s2-sync/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
