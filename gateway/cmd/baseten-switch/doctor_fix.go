// doctor_fix.go implements `baseten-switch doctor --fix`, the interactive
// repair loop over the read-only chain diagnostic (doctor.go). Plain
// doctor stays strictly read-only; --fix mutates ONLY by running
// existing baseten-switch verbs (the fixArgv attached to a check) as
// confirmed self-exec child processes, so every repair is a command
// the user could have typed. Warns are never auto-fixed: the loop
// consults only the FIRST FAILED check each round.
//
// Loop shape: run the chain, print the report; while the first failed
// check carries a fixArgv, show the exact command, confirm (--yes
// auto-confirms), run it streaming, re-run the chain. A fix that does
// not clear its own check stops the loop ("fix did not resolve"), as
// does a manual-only first failure, a decline, or the round cap
// (doctorFixMaxRounds, against ping-ponging fixes). The exit code is
// the FINAL chain state: 0 no failures, 1 failures remain.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// doctorFixMaxRounds caps applied fixes per --fix invocation so two
// fixes that keep undoing each other cannot loop forever.
const doctorFixMaxRounds = 5

// doctorFixGuardEnv is set in every fix child's environment; cmdDoctor
// refuses --fix when it is already set, so a verb that itself reaches
// doctor --fix cannot recurse.
const doctorFixGuardEnv = "BASETEN_SWITCH_DOCTOR_FIX"

// Seams so tests never exec a real verb, read a real terminal, or
// depend on the test runner's stdin being a TTY.
var (
	// doctorFixExec runs one fix argv against this executable,
	// streaming its output, with the recursion guard set.
	doctorFixExec = func(argv []string) error {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve self executable: %v", err)
		}
		cmd := exec.Command(self, argv...)
		cmd.Stdin = nil
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = envWith(map[string]string{doctorFixGuardEnv: "1"})
		return cmd.Run()
	}

	// doctorFixIn supplies the confirmation answers.
	doctorFixIn io.Reader = os.Stdin

	// doctorStdinIsTTY gates --fix without --yes: prompts need an
	// interactive terminal (isTerminal, doctor_fix_tty_*.go).
	doctorStdinIsTTY = func() bool { return isTerminal(os.Stdin) }
)

// runDoctorFix drives the repair loop. Precondition (cmdDoctor): not
// under the recursion guard, and either --yes or a TTY stdin.
func runDoctorFix(o doctorOpts, verbose bool) int {
	in := bufio.NewReader(doctorFixIn)
	applied := 0
	lastFixed := "" // check id (section/name) the previous round tried to fix
	for {
		w := os.Stdout
		rep := runDoctor(o)
		printDoctorReport(w, rep, verbose)
		if rep.FirstFailure == "" {
			return 0
		}
		if lastFixed != "" && doctorCheckFails(rep, lastFixed) {
			fmt.Fprintf(w, "fix did not resolve %s; stopping\n", lastFixed)
			return rep.ExitCode
		}
		lastFixed = ""
		first := doctorFindCheck(rep, rep.FirstFailure)
		if len(first.fixArgv) == 0 {
			fmt.Fprintf(w, "no automatable fix for %s\n", rep.FirstFailure)
			doctorPrintManualFix(w, first.Fix)
			return rep.ExitCode
		}
		if applied >= doctorFixMaxRounds {
			fmt.Fprintf(w, "repair round cap (%d) reached; the remaining failures need manual fixes\n", doctorFixMaxRounds)
			doctorPrintManualFix(w, first.Fix)
			return rep.ExitCode
		}
		cmdline := doctorFixCommand(first.fixArgv)
		fmt.Fprintf(w, "fix available for %s: %s\n  will run: %s\n", rep.FirstFailure, first.Finding, cmdline)
		if o.yes {
			fmt.Fprintln(w, "applying (--yes)")
		} else {
			fmt.Fprint(w, "apply? [y/N] ")
			line, _ := in.ReadString('\n')
			if !strings.EqualFold(strings.TrimSpace(line), "y") {
				fmt.Fprintln(w, "not applied")
				doctorPrintManualFix(w, first.Fix)
				return rep.ExitCode
			}
		}
		applied++
		lastFixed = rep.FirstFailure
		if err := doctorFixExec(first.fixArgv); err != nil {
			// The chain re-run decides the outcome; a failed command
			// normally surfaces as "fix did not resolve".
			fmt.Fprintf(w, "fix command failed: %v\n", err)
		}
		fmt.Fprintf(w, "re-running the chain\n\n")
	}
}

// doctorFixCommand renders the exact command a fix will run.
func doctorFixCommand(argv []string) string {
	self, err := os.Executable()
	if err != nil {
		self = "baseten-switch"
	}
	return self + " " + strings.Join(argv, " ")
}

func doctorPrintManualFix(w io.Writer, fix string) {
	if fix != "" {
		fmt.Fprintf(w, "  manual fix: %s\n", fix)
	}
}

// doctorFindCheck returns the check with the given section/name id;
// the zero check when absent.
func doctorFindCheck(rep doctorReport, id string) doctorCheck {
	for _, c := range rep.Checks {
		if c.Section+"/"+c.Name == id {
			return c
		}
	}
	return doctorCheck{}
}

func doctorCheckFails(rep doctorReport, id string) bool {
	return doctorFindCheck(rep, id).Status == docFail
}
