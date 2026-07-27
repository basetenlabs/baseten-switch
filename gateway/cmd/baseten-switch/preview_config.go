package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const previewConfigUsage = "usage: baseten-switch config preview-snapshot --source PATH --output PATH --root PATH --router-addr HOST:PORT --door-addr HOST:PORT | baseten-switch config preview-validate --path PATH --root PATH --router-addr HOST:PORT --door-addr HOST:PORT"

type previewConfigArgs struct {
	Source     string
	Output     string
	Path       string
	Root       string
	RouterAddr string
	DoorAddr   string
}

func cmdConfigPreviewSnapshot(args []string) int {
	parsed, err := parsePreviewConfigArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if parsed.Source == "" || parsed.Output == "" || parsed.Path != "" {
		fmt.Fprintln(os.Stderr, previewConfigUsage)
		return 2
	}
	return runConfigPreviewSnapshot(parsed, os.Stderr)
}

func cmdConfigPreviewValidate(args []string) int {
	parsed, err := parsePreviewConfigArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if parsed.Path == "" || parsed.Source != "" || parsed.Output != "" {
		fmt.Fprintln(os.Stderr, previewConfigUsage)
		return 2
	}
	return runConfigPreviewValidate(parsed, os.Stderr)
}

func parsePreviewConfigArgs(args []string) (previewConfigArgs, error) {
	var out previewConfigArgs
	for i := 0; i < len(args); i++ {
		name := args[i]
		var target *string
		switch name {
		case "--source":
			target = &out.Source
		case "--output":
			target = &out.Output
		case "--path":
			target = &out.Path
		case "--root":
			target = &out.Root
		case "--router-addr":
			target = &out.RouterAddr
		case "--door-addr":
			target = &out.DoorAddr
		default:
			if strings.HasPrefix(name, "--") {
				return out, fmt.Errorf("unknown Preview config flag: %s", name)
			}
			return out, fmt.Errorf("unexpected Preview config argument: %s", name)
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return out, fmt.Errorf("%s requires a value", name)
		}
		i++
		*target = args[i]
	}
	if out.Root == "" || out.RouterAddr == "" || out.DoorAddr == "" {
		return out, fmt.Errorf("--root, --router-addr, and --door-addr are required")
	}
	return out, nil
}

func (a previewConfigArgs) policy() config.PreviewPolicy {
	return config.PreviewPolicy{
		Root:       a.Root,
		RouterAddr: a.RouterAddr,
		DoorAddr:   a.DoorAddr,
	}
}

func runConfigPreviewSnapshot(args previewConfigArgs, errOut io.Writer) int {
	source, err := config.Load(args.Source)
	if err != nil {
		fmt.Fprintf(errOut, "config preview-snapshot: %v\n", err)
		return 1
	}
	preview, err := config.BuildPreviewConfig(source, args.policy())
	if err != nil {
		fmt.Fprintf(errOut, "config preview-snapshot: %v\n", err)
		return 1
	}
	if err := config.Save(args.Output, preview); err != nil {
		fmt.Fprintf(errOut, "config preview-snapshot: save %s: %v\n", args.Output, err)
		return 1
	}
	fmt.Fprintf(errOut, "config preview-snapshot: wrote isolated config %s\n", args.Output)
	return 0
}

func runConfigPreviewValidate(args previewConfigArgs, errOut io.Writer) int {
	file, err := config.Load(args.Path)
	if err != nil {
		fmt.Fprintf(errOut, "config preview-validate: %v\n", err)
		return 1
	}
	if err := config.ValidatePreviewConfig(file, args.policy()); err != nil {
		fmt.Fprintf(errOut, "config preview-validate: %v\n", err)
		return 1
	}
	return 0
}
