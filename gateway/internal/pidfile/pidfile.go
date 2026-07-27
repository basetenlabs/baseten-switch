package pidfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func Path() string {
	if p := os.Getenv("BASETEN_SWITCH_GATEWAY_PIDFILE"); p != "" {
		return p
	}
	return configDirJoin("gateway.pid", "baseten-switch-gateway.pid")
}

// DoorPath is the front-door process pidfile, written by the `door`
// subcommand itself so `down` can manage hand-started doors too.
// Env-overridable so tests and scratch boots never touch the real one.
func DoorPath() string {
	if p := os.Getenv("BASETEN_SWITCH_DOOR_PIDFILE"); p != "" {
		return p
	}
	return configDirJoin("door.pid", "baseten-switch-door-process.pid")
}

func configDirJoin(name, tmpName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), tmpName)
	}
	return filepath.Join(home, ".config", "baseten-switch", name)
}

func Write(pid int) error {
	p := Path()
	return WriteAt(p, pid)
}

func WriteAt(p string, pid int) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
}

func Read() (int, error) {
	return ReadFrom(Path())
}

func ReadFrom(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("pidfile corrupt: %q", s)
	}
	return pid, nil
}

func ReadFromSafe(p string) int {
	pid, err := ReadFrom(p)
	if err != nil {
		return 0
	}
	return pid
}

// ConfigStatePath returns the path of the config-path state file that
// lives next to the given pidfile. The gateway records the config file
// it resolved at startup there so a later `gateway start` without an
// explicit BASETEN_SWITCH_CONFIG_PATH can reuse it instead of silently switching
// to the default path. It is memory of last intent, not a lock: stop
// leaves it in place.
func ConfigStatePath(pidPath string) string {
	return filepath.Join(filepath.Dir(pidPath), "gateway.config-path")
}

func WriteConfigState(pidPath, configPath string) error {
	p := ConfigStatePath(pidPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(configPath+"\n"), 0o644)
}

func ReadConfigState(pidPath string) (string, error) {
	b, err := os.ReadFile(ConfigStatePath(pidPath))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func IsAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func Unlink() {
	os.Remove(Path())
}

func UnlinkAt(p string) {
	os.Remove(p)
}
