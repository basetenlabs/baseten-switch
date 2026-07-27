// auth_login.go implements `baseten-switch auth login`: shell out to the
// baseten CLI's device flow, then SIGHUP the running router so it
// rebuilds its oauth client from the fresh credential. Shelling out
// keeps the CLI the single writer of credential-store formats
// (the credential-refresh contract, "Triggering reauth"); baseten-switch
// deliberately never writes auth.json or the keyring.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// basetenBrewHint names the install command for the baseten CLI.
const basetenBrewHint = "brew install basetenlabs/baseten/baseten"

// lookBaseten resolves the baseten CLI from PATH. Package var so the
// no-binary case stays deterministic in tests regardless of the host.
var lookBaseten = func() (string, error) { return exec.LookPath("baseten") }

// runBasetenLogin runs `<bin> auth login` with inherited stdio: the
// device flow prints a verification URL, may open a browser, and reads
// confirmation from the terminal, so the child owns all three streams
// for its duration. Package var so verb tests can fake the login
// outcome without a real device flow.
var runBasetenLogin = func(bin string) error {
	cmd := exec.Command(bin, "auth", "login")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// printLoginIdentity prints the post-login identity exactly like
// `baseten-switch whoami`. Package var so verb tests skip the network call.
var printLoginIdentity = func() int { return cmdWhoami(nil) }

func cmdAuth(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch auth login")
		return 2
	}
	switch args[0] {
	case "login":
		return cmdAuthLogin(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown auth subcommand: %s\n", args[0])
		return 2
	}
}

func cmdAuthLogin(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch auth login")
		return 2
	}
	bin, err := lookBaseten()
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth login: no 'baseten' CLI on PATH (the CLI owns the credential store; baseten-switch never writes it). Install it with '%s'.\n", basetenBrewHint)
		return 1
	}
	// Name the binary and version about to write the store: separate
	// baseten installations can create ambiguous credential ownership, so
	// which binary ran matters.
	if v := basetenCLIVersion(bin); v != "" {
		fmt.Fprintf(os.Stderr, "auth login: running %s (%s)\n", bin, v)
	} else {
		fmt.Fprintf(os.Stderr, "auth login: running %s\n", bin)
	}
	if err := runBasetenLogin(bin); err != nil {
		// The child owned stderr, so its own error text already printed;
		// pass its exit code through untouched.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "auth login: %v\n", err)
		return 1
	}
	// Reload the running router now. The router also self-heals (its
	// dead- and signed-out-state store watches converge within ~30s),
	// but a SIGHUP closes the window where baseten-routed requests keep
	// replaying the dead token.
	switch state, pid := classifyPidfile(gatewayPidfilePath()); state {
	case pidfileAlive:
		if err := signalRouter(pid); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not SIGHUP router pid %d: %v; it reloads the credential on its own within ~30s, or run 'baseten-switch restart'\n", pid, err)
		} else {
			fmt.Fprintf(os.Stderr, "router reloaded (SIGHUP pid %d)\n", pid)
		}
	default:
		fmt.Fprintln(os.Stderr, "router not running (per its pidfile); the new credential loads at the next start (a router that is in fact running picks it up on its own within ~30s)")
	}
	return printLoginIdentity()
}
