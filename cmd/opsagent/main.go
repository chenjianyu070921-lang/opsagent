package main

import (
	"context"
	"os"

	"opsagent/internal/cli"
)

func main() {
	if err := cli.Execute(context.Background()); err != nil {
		os.Exit(1)
	}
}
