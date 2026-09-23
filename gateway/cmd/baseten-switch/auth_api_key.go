package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
)

func cmdAuthAPIKey(args []string, input io.Reader, out, errOut io.Writer) int {
	if len(args) != 1 || (args[0] != "set" && args[0] != "remove") {
		fmt.Fprintln(errOut, "usage: baseten-switch auth api-key set < key-file | baseten-switch auth api-key remove")
		return 2
	}
	path, _ := resolveConfigPath()
	var mutationErr error
	if args[0] == "set" {
		const maxInput = 4096
		raw, err := io.ReadAll(io.LimitReader(input, maxInput+1))
		if err != nil || len(raw) > maxInput {
			fmt.Fprintln(errOut, "auth api-key: could not read API key from stdin (maximum 4096 bytes)")
			return 1
		}
		mutationErr = auth.SaveSavedAPIKey(path, strings.TrimSpace(string(raw)))
		if mutationErr == nil {
			fmt.Fprintln(out, "Saved API key. Switch prioritizes it over Baseten CLI authentication.")
		}
	} else {
		mutationErr = auth.RemoveSavedAPIKey(path)
		if mutationErr == nil {
			fmt.Fprintln(out, "Removed saved API key. Switch uses Baseten CLI authentication when available.")
		}
	}
	if mutationErr != nil {
		fmt.Fprintf(errOut, "auth api-key: %v\n", mutationErr)
	}
	if state, pid := classifyPidfile(gatewayPidfilePath()); state == pidfileAlive {
		if err := signalRouter(pid); err != nil {
			fmt.Fprintln(errOut, "The router could not be reloaded. Send SIGHUP to reconcile its API key setting.")
			return 1
		}
		fmt.Fprintln(out, "Router reloaded (SIGHUP).")
	} else if mutationErr == nil {
		fmt.Fprintln(out, "Router not running; the setting applies on the next start.")
	}
	if mutationErr != nil {
		return 1
	}
	return 0
}
