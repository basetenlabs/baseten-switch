package pidfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.pid")
	if _, err := ReadFrom(p); err == nil {
		t.Fatal("expected error reading missing pidfile")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "gw.pid")
	if err := WriteAt(p, 123456); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != 123456 {
		t.Fatalf("got %d want 123456", got)
	}
}

func TestCorruptPidfile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pid")
	if err := os.WriteFile(p, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrom(p); err == nil {
		t.Fatal("expected error for corrupt pidfile")
	}
}

func TestStalePidNotAlive(t *testing.T) {
	pid := 9999999
	for pid == os.Getpid() {
		pid++
	}
	if IsAlive(pid) {
		t.Fatal("expected stale pid to be not alive")
	}
}

func TestSelfAlive(t *testing.T) {
	if !IsAlive(os.Getpid()) {
		t.Fatal("expected own pid to be alive")
	}
}

func TestUnlinkRemoves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rm.pid")
	_ = WriteAt(p, os.Getpid())
	UnlinkAt(p)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("pidfile still exists: %v", err)
	}
}

func TestConfigStatePathNextToPidfile(t *testing.T) {
	got := ConfigStatePath("/some/dir/gateway.pid")
	want := filepath.Join("/some/dir", "gateway.config-path")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfigStateRoundTrip(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "nested", "gw.pid")
	if err := WriteConfigState(pf, "/tmp/baseten-qa/gateway.yaml"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfigState(pf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/baseten-qa/gateway.yaml" {
		t.Fatalf("got %q", got)
	}
}

func TestConfigStateMissing(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "gw.pid")
	if _, err := ReadConfigState(pf); err == nil {
		t.Fatal("expected error for missing config state file")
	}
}

func TestConfigStateOverwrite(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "gw.pid")
	if err := WriteConfigState(pf, "/first.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigState(pf, "/second.yaml"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfigState(pf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/second.yaml" {
		t.Fatalf("got %q want /second.yaml", got)
	}
}

// TestDoorPidfilePath pins the second active process pidfile location.
func TestDoorPidfilePath(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		fn      func() string
		defName string
	}{
		{"door", "BASETEN_SWITCH_DOOR_PIDFILE", DoorPath, "door.pid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override := filepath.Join(t.TempDir(), "custom.pid")
			t.Setenv(tc.env, override)
			if got := tc.fn(); got != override {
				t.Fatalf("%s override: got %q want %q", tc.env, got, override)
			}
			t.Setenv(tc.env, "")
			got := tc.fn()
			if filepath.Base(got) != tc.defName {
				t.Fatalf("default basename: got %q want %q", got, tc.defName)
			}
			if home, err := os.UserHomeDir(); err == nil {
				want := filepath.Join(home, ".config", "baseten-switch", tc.defName)
				if got != want {
					t.Fatalf("default path: got %q want %q", got, want)
				}
			}
		})
	}
}

// TestDoorPidfileWriteRead exercises the door pidfile round trip the
// orchestrator relies on for the second process.
func TestDoorPidfileWriteRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "door.pid")
	t.Setenv("BASETEN_SWITCH_DOOR_PIDFILE", p)
	if err := WriteAt(DoorPath(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrom(DoorPath())
	if err != nil {
		t.Fatal(err)
	}
	if got != os.Getpid() {
		t.Fatalf("got %d want %d", got, os.Getpid())
	}
	if !IsAlive(got) {
		t.Fatal("expected own pid alive")
	}
	UnlinkAt(DoorPath())
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("door pidfile still exists after unlink")
	}
}

func ensureDying(t *testing.T, pid int) {
	proc, _ := os.FindProcess(pid)
	for i := 0; i < 50; i++ {
		if !IsAlive(pid) {
			return
		}
		_ = proc.Signal(syscall.Signal(0))
		time.Sleep(20 * time.Millisecond)
	}
}
