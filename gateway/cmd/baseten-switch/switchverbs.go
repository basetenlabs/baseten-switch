// switchverbs.go implements the one top-level routing switch:
// `baseten-switch on` and `baseten-switch off`. Both edit only the required
// global.routing_enabled field.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

// signalRouter sends SIGHUP to the router process so a flipped config
// hot-reloads. A package var so unit tests can substitute a recorder
// and never signal a real process.
var signalRouter = func(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }

// routeApplyTimeout bounds the post-SIGHUP wait for activation.
var routeApplyTimeout = 4 * time.Second

func cmdOn(args []string) int  { return runSwitch("on", args, os.Stdout) }
func cmdOff(args []string) int { return runSwitch("off", args, os.Stdout) }

func runSwitch(verb string, args []string, out io.Writer) int {
	requested := verb == "on"
	opts, positional, err := parseMutationOptions(args)
	if err != nil {
		return failMutation(opts, out, mutationResult{
			Operation: "set_global_routing",
			Requested: requested,
		}, "usage", fmt.Sprintf("usage: baseten-switch %s [mutation options]: %v", verb, err), false, 2)
	}
	if len(positional) != 0 {
		return failMutation(opts, out, mutationResult{
			Operation: "set_global_routing",
			Requested: requested,
		}, "client_scoped_switch_removed",
			fmt.Sprintf("baseten-switch %s takes no client argument; routing has one global switch (use 'baseten-switch claude route' for model mappings)", verb),
			false, 2)
	}
	path, notices := resolveConfigPath()
	if !opts.JSON {
		for _, notice := range notices {
			fmt.Fprintln(os.Stderr, notice)
		}
	}
	if _, err := config.Load(path); err != nil {
		message := config.MalformedConfigMessage(path, err)
		if errors.Is(err, os.ErrNotExist) {
			message = config.MissingConfigMessage(path)
		}
		return failMutation(opts, out, mutationResult{
			Operation:  "set_global_routing",
			Requested:  requested,
			ConfigPath: path,
		}, "config_load_failed", message, false, 1)
	}

	lock, err := acquireConfigMutationLock(path)
	if err != nil {
		return failMutation(opts, out, mutationResult{
			Operation:  "set_global_routing",
			Requested:  requested,
			ConfigPath: path,
		}, "mutation_locked", err.Error(), true, 1)
	}
	defer lock.close()
	if err := recoverInterruptedExactConfigCommit(path); err != nil {
		return failMutation(opts, out, mutationResult{
			Operation:  "set_global_routing",
			Requested:  requested,
			ConfigPath: path,
		}, "commit_recovery_failed", err.Error(), true, 1)
	}
	prior, mode, err := readExactConfig(path)
	if err != nil {
		return failMutation(opts, out, mutationResult{
			Operation:  "set_global_routing",
			Requested:  requested,
			ConfigPath: path,
		}, "config_read_failed", fmt.Sprintf("read %s: %v", path, err), true, 1)
	}
	file, err := config.Load(path)
	if err != nil {
		return failMutation(opts, out, mutationResult{
			Operation:  "set_global_routing",
			Requested:  requested,
			ConfigPath: path,
		}, "config_load_failed", config.MalformedConfigMessage(path, err), false, 1)
	}
	return runGlobalSwitchLocked(verb, path, file, prior, mode, opts, out)
}

func clientNames(file *config.File) []string {
	out := make([]string, 0, len(file.Clients))
	for _, client := range file.Clients {
		out = append(out, client.Name)
	}
	return out
}
