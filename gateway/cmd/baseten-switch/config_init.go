// config_init.go implements `baseten-switch config init [--force]`, the
// commands-only config generator used by `baseten-switch setup`. It writes
// config.InitTemplate, a go:embed copy pinned byte-for-byte to
// config/gateway.example.yaml, to the
// resolved config path, then proves the written file loads and
// resolves (config.Load, door.SpecsFromConfig, the claude adapter's
// door-port resolution) before printing the next-step commands.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
)

func cmdConfigInit(args []string) int {
	force := false
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s (usage: baseten-switch config init [--force])\n", a)
			return 2
		}
	}
	path := envDefault("BASETEN_SWITCH_CONFIG_PATH", config.DefaultPath())
	return runConfigInit(path, force, os.Stderr)
}

// runConfigInit writes the default config to path. An existing file is
// a refusal unless force is set, in which case the old file is backed
// up to <path>.bak first. Parent directories are created 0755, the
// file 0600.
func runConfigInit(path string, force bool, out io.Writer) int {
	if _, err := os.Stat(path); err == nil {
		if !force {
			fmt.Fprintf(out, "config init: %s already exists; refusing to overwrite it. Rerun with --force to replace it (the current file is backed up to %s.bak first).\n", path, path)
			return 1
		}
		orig, rerr := os.ReadFile(path)
		if rerr != nil {
			fmt.Fprintf(out, "config init: read existing config for backup: %v\n", rerr)
			return 1
		}
		bak := path + ".bak"
		if werr := os.WriteFile(bak, orig, 0o600); werr != nil {
			fmt.Fprintf(out, "config init: backup: %v\n", werr)
			return 1
		}
		fmt.Fprintf(out, "backed up existing config to %s\n", bak)
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(out, "config init: stat %s: %v\n", path, err)
		return 1
	}

	if err := writeInitConfig(path); err != nil {
		fmt.Fprintf(out, "config init: %v\n", err)
		return 1
	}

	// Validate what was just written: it must load, the door section
	// must resolve to specs, and the claude adapter must find the door
	// port it points harnesses at. Failure here is a baseten-switch bug (a
	// broken template), never a user error.
	f, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(out, "config init: wrote %s but it does not load: %v (this is a baseten-switch bug; please report it)\n", path, err)
		return 1
	}
	specs, err := door.SpecsFromConfig(f, door.SpecsOptions{Logf: func(string, ...any) {}})
	if err != nil {
		fmt.Fprintf(out, "config init: wrote %s but the door section does not resolve: %v (this is a baseten-switch bug; please report it)\n", path, err)
		return 1
	}
	if _, _, _, err := claudeDoorPort(f); err != nil {
		fmt.Fprintf(out, "config init: wrote %s but the claude adapter cannot resolve a door port from it: %v (this is a baseten-switch bug; please report it)\n", path, err)
		return 1
	}

	fmt.Fprintf(out, "wrote %s (single-port door topology: door %s -> router %s)\n", path, specs[0].ListenAddr, specs[0].RouterTarget)
	fmt.Fprintf(out, "\nNext steps:\n")
	fmt.Fprintf(out, "  1. baseten auth login       OAuth sign-in (preferred; no env file needed), or put\n")
	fmt.Fprintf(out, "                              BASETEN_API_KEY=<key> and BASETEN_SWITCH_API_KEY_FALLBACK=1 in %s (mode 0600)\n", config.EnvFilePath())
	fmt.Fprintf(out, "  2. baseten-switch up --install   start router + door with launchd supervision (plain 'baseten-switch up' to skip launchd)\n")
	fmt.Fprintf(out, "  3. baseten-switch claude on      point Claude Code at the gateway door\n")
	fmt.Fprintf(out, "  4. baseten-switch status         verify everything is up\n")
	return 0
}

// writeInitConfig creates missing parent directories (chmod 0755 on
// each one it created, umask-proof), then writes the template
// atomically (temp file + rename) with mode 0600.
func writeInitConfig(path string) error {
	dir := filepath.Dir(path)
	var created []string
	for d := dir; ; {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", d, err)
		}
		created = append(created, d)
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for _, d := range created {
		if err := os.Chmod(d, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", d, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".gateway.yaml.init.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(config.InitTemplate); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
