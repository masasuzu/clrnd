package main

import (
	"os"

	"github.com/masasuzu/crner/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
