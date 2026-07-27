package launchd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	j := Job{
		Label:            RouterLabel,
		ProgramArguments: []string{"/Users/x/.local/bin/baseten-switch", "gateway", "start", "--foreground"},
		Env: map[string]string{
			"PATH":                       "/usr/bin:/bin",
			"BASETEN_SWITCH_CONFIG_PATH": "/Users/x/.config/baseten-switch/gateway.yaml",
		},
		LogPath: "/Users/x/.config/baseten-switch/logs/router.log",
	}
	got := RenderPlist(j)
	wants := []string{
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>",
		"<key>Label</key>",
		"<string>co.baseten.switch.router</string>",
		"<key>ProgramArguments</key>",
		"<string>/Users/x/.local/bin/baseten-switch</string>",
		"<string>gateway</string>",
		"<string>start</string>",
		"<string>--foreground</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		// Background Activity grouping under the menubar app: array
		// form, both agents (the lifecycle contract).
		"<key>AssociatedBundleIdentifiers</key>\n\t<array>\n\t\t<string>" + ToggleBundleID + "</string>\n\t</array>",
		"<key>EnvironmentVariables</key>",
		"<key>BASETEN_SWITCH_CONFIG_PATH</key>",
		"<string>/Users/x/.config/baseten-switch/gateway.yaml</string>",
		"<key>PATH</key>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
		"<string>/Users/x/.config/baseten-switch/logs/router.log</string>",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plist missing %q:\n%s", w, got)
		}
	}
	// Env keys render sorted: BASETEN_SWITCH_CONFIG_PATH before PATH.
	if strings.Index(got, "BASETEN_SWITCH_CONFIG_PATH") > strings.Index(got, "<key>PATH</key>") {
		t.Errorf("env keys not sorted:\n%s", got)
	}
}

func TestRenderPlistEscapes(t *testing.T) {
	j := Job{
		Label:            DoorLabel,
		ProgramArguments: []string{"/tmp/a&b <dir>/baseten-switch", "door"},
		Env:              map[string]string{"BASETEN_SWITCH_CONFIG_PATH": `/tmp/it's "here".yaml`},
		LogPath:          "/tmp/log",
	}
	got := RenderPlist(j)
	for _, w := range []string{"a&amp;b &lt;dir&gt;", "it&apos;s &quot;here&quot;"} {
		if !strings.Contains(got, w) {
			t.Errorf("plist missing escaped %q:\n%s", w, got)
		}
	}
	for _, bad := range []string{"a&b", `"here"`} {
		if strings.Contains(got, bad) {
			t.Errorf("plist contains unescaped %q:\n%s", bad, got)
		}
	}
}

func TestRewriteCellar(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/opt/homebrew/Cellar/baseten-switch/0.3.0/bin/baseten-switch", "/opt/homebrew/opt/baseten-switch/bin/baseten-switch"},
		{"/usr/local/Cellar/baseten-switch/1.0.0_1/bin/baseten-switch", "/usr/local/opt/baseten-switch/bin/baseten-switch"},
		{"/Users/x/.local/bin/baseten-switch", "/Users/x/.local/bin/baseten-switch"},
		{"/opt/homebrew/Cellar/baseten-switch", "/opt/homebrew/Cellar/baseten-switch"}, // no version/rest: unchanged
		{"/opt/homebrew/bin/baseten-switch", "/opt/homebrew/bin/baseten-switch"},
	}
	for _, tc := range cases {
		if got := RewriteCellar(tc.in); got != tc.want {
			t.Errorf("RewriteCellar(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// launchctl print gui/<uid> service-list excerpts in both observed
// formats, plus label mentions in other sections.
const printQuoted = `system information:
	services = {
		"com.apple.something" => {
			pid = 42
		}
		"homebrew.mxcl.baseten-switch" => {
			pid = 99
		}
	}
`

const printColumnar = `	services = {
		0	-	com.apple.SafariHistoryServiceAgent
		4321	0	co.baseten.switch.router
		-	0	co.baseten.switch.door
	}
	disabled services = {
		"co.baseten.switch.door" => disabled
	}
	plist path = /Users/x/Library/LaunchAgents/co.baseten.switch.router.plist
`

func TestLabelsMentioning(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []string
	}{
		{"brew label quoted", printQuoted, []string{"homebrew.mxcl.baseten-switch"}},
		{"ours columnar dedup, paths ignored", printColumnar,
			[]string{"co.baseten.switch.door", "co.baseten.switch.router"}},
		{"no match", "services = {\n 0 - com.apple.Foo\n}", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LabelsMentioning(tc.out, "baseten-switch")
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

type fakeRunner struct {
	out   string
	err   error
	calls [][]string
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

func TestBootoutTolerance(t *testing.T) {
	t.Run("loaded", func(t *testing.T) {
		r := &fakeRunner{}
		was, err := Bootout(r, DoorLabel)
		if err != nil || !was {
			t.Fatalf("was=%v err=%v", was, err)
		}
	})
	t.Run("not loaded tolerated", func(t *testing.T) {
		r := &fakeRunner{out: "Boot-out failed: 3: No such process\n", err: errors.New("launchctl bootout: exit status 3")}
		was, err := Bootout(r, DoorLabel)
		if err != nil {
			t.Fatalf("expected tolerated not-loaded, got %v", err)
		}
		if was {
			t.Fatal("wasLoaded should be false")
		}
	})
	t.Run("real failure surfaces", func(t *testing.T) {
		r := &fakeRunner{out: "Boot-out failed: 5: Input/output error\n", err: errors.New("launchctl bootout: exit status 5")}
		if _, err := Bootout(r, DoorLabel); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestLoaded(t *testing.T) {
	r := &fakeRunner{}
	if !Loaded(r, RouterLabel) {
		t.Fatal("expected loaded")
	}
	if len(r.calls) != 1 || r.calls[0][0] != "print" || !strings.HasSuffix(r.calls[0][1], "/"+RouterLabel) {
		t.Fatalf("unexpected calls %v", r.calls)
	}
	r2 := &fakeRunner{err: errors.New("exit status 113")}
	if Loaded(r2, RouterLabel) {
		t.Fatal("expected not loaded")
	}
}

// TestProgramBinary: the adoption path reads the binary a plist would
// launch back out of RenderPlist's own output, XML escaping included.
func TestProgramBinary(t *testing.T) {
	j := Job{
		Label:            RouterLabel,
		ProgramArguments: []string{"/opt/dir with & ampersand/baseten-switch", "gateway", "start"},
		LogPath:          "/tmp/router.log",
	}
	pp := filepath.Join(t.TempDir(), RouterLabel+".plist")
	if err := os.WriteFile(pp, []byte(RenderPlist(j)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProgramBinary(pp); got != j.ProgramArguments[0] {
		t.Fatalf("ProgramBinary = %q want %q", got, j.ProgramArguments[0])
	}
	if got := ProgramBinary(filepath.Join(t.TempDir(), "absent.plist")); got != "" {
		t.Fatalf("ProgramBinary on a missing file = %q want empty", got)
	}
	junk := filepath.Join(t.TempDir(), "junk.plist")
	if err := os.WriteFile(junk, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProgramBinary(junk); got != "" {
		t.Fatalf("ProgramBinary on junk = %q want empty", got)
	}
}
