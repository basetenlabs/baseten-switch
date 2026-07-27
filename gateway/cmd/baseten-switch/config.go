package main

import (
	"fmt"
	"os"
)

func cmdConfig(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "init":
		return cmdConfigInit(args[1:])
	case "reset":
		return cmdConfigReset(args[1:])
	case "preview-snapshot":
		return cmdConfigPreviewSnapshot(args[1:])
	case "preview-validate":
		return cmdConfigPreviewValidate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand: %s\n", args[0])
		return 2
	}
}

const configUsage = "usage: baseten-switch config init [--force] | baseten-switch config reset --yes [--preview-root PATH --router-addr HOST:PORT --door-addr HOST:PORT]"
