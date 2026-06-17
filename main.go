package main

import (
	"os"

	"github.com/masasuzu/clrnd/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
