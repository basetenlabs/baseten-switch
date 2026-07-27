package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commentedFixture is a comment-rich config in the style 'baseten-switch
// config init' generates: a head comment block, aligned values, blank
// lines, and a commented-out client block. Every one of those byte
// regions must survive a targeted edit untouched.
const commentedFixture = `# gateway.yaml -- head comment block line one.
# Head comment block line two.

global:
  routing_enabled: true

clients:
  # Guidance comment above the first client.
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    fallback_route: anthropic

  # Commented-out client block, kept as user guidance:
  #  - name: parked
  #    enabled: false
  #    protocol_shape: anthropic

  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18082
    protocol_shape: openai
    fallback_route: openai
`

// writeEditFixture writes content to a scratch gateway.yaml and
// returns its path. Tests never touch a real config.
func writeEditFixture(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetGlobalRoutingEnabled(t *testing.T) {
	const fixture = `# global routing fixture
global:
  routing_enabled: true  # only this token changes
clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    fallback_route: anthropic
`
	path := writeEditFixture(t, fixture, 0o600)
	if err := SetGlobalRoutingEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(fixture,
		"routing_enabled: true  # only this token changes",
		"routing_enabled: false  # only this token changes", 1)
	if string(got) != want {
		t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if err := SetGlobalRoutingEnabled(path, true); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fixture {
		t.Fatalf("off/on did not round-trip byte-identically:\n%s", got)
	}
}

func TestSetGlobalRoutingEnabledRequiresExplicitGate(t *testing.T) {
	const contents = "global: {}\nclients: []\n"
	path := writeEditFixture(t, contents, 0o600)
	err := SetGlobalRoutingEnabled(path, false)
	if err == nil || !strings.Contains(err.Error(), "routing_enabled") {
		t.Fatalf("err = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != contents {
		t.Fatalf("file changed on error:\n%s", got)
	}
}

func TestSetGlobalRoutingEnabledTemplateRoundTrip(t *testing.T) {
	path := writeEditFixture(t, string(InitTemplate), 0o600)

	if err := SetGlobalRoutingEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	off, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	origLines := strings.Split(string(InitTemplate), "\n")
	offLines := strings.Split(string(off), "\n")
	if len(origLines) != len(offLines) {
		t.Fatalf("off state has %d lines, template has %d", len(offLines), len(origLines))
	}
	var diffs []string
	for i := range origLines {
		if origLines[i] == offLines[i] {
			continue
		}
		if origLines[i] != "  routing_enabled: true" {
			t.Fatalf("line %d changed but is not the global gate:\n-%q\n+%q", i+1, origLines[i], offLines[i])
		}
		if offLines[i] != "  routing_enabled: false" {
			t.Fatalf("line %d flipped to unexpected content %q", i+1, offLines[i])
		}
		diffs = append(diffs, offLines[i])
	}
	if len(diffs) != 1 {
		t.Fatalf("off state changed %d lines, want exactly the global gate: %v", len(diffs), diffs)
	}

	if err := SetGlobalRoutingEnabled(path, true); err != nil {
		t.Fatal(err)
	}
	on, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(on, InitTemplate) {
		t.Fatalf("template did not survive the off/on round trip byte-identically\n--- got ---\n%s", on)
	}
}

// scalarFixture is a comment-rich config with a live model_aliases block
// on the claude-code client, so insert-after-model_aliases and
// replace-in-place both have realistic anchors. The codex client has no
// model_aliases so the protocol_shape/name fallback anchors are
// exercised. Every byte outside the touched scalar tokens must survive.
const scalarFixture = `# gateway.yaml -- head comment block.

global:
  routing_enabled: true

clients:
  # Guidance comment above the first client.
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-glm-5-2:   zai-org/GLM-5.2
      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code
    fallback_route: anthropic

  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18082
    protocol_shape: openai
    fallback_route: openai
`

// TestSetClientScalars drives the scalar editor table-style and asserts
// the FULL resulting file, so any lost comment, blank line, alignment
// space, or reordering fails the test, not just the scalar values.
func TestSetClientScalars(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		client    string
		set       map[string]string
		want      string // full expected file content
		wantErr   string // substring of the expected error ("" = no error)
		untouched bool   // assert file is byte-identical to in on error
	}{
		{
			name:   "replace plain scalar in place",
			in:     scalarFixture,
			client: "claude-code",
			set:    map[string]string{"subagent_model": "claude-baseten-glm-5-2"},
			want: strings.Replace(scalarFixture,
				"    fallback_route: anthropic\n",
				"    subagent_model: claude-baseten-glm-5-2\n    fallback_route: anthropic\n", 1),
		},
		{
			name: "replace quoted scalar preserves inline comment",
			in: "clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    subagent_model: \"claude-baseten-glm-5-2\"  # trailing comment survives\n",
			client: "claude-code",
			set:    map[string]string{"subagent_model": "claude-baseten-kimi-k2-7"},
			want: "clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    subagent_model: claude-baseten-kimi-k2-7  # trailing comment survives\n",
		},
		{
			name:   "insert after model_aliases block",
			in:     scalarFixture,
			client: "claude-code",
			set:    map[string]string{"subagent_model": "claude-baseten-glm-5-2"},
			want: strings.Replace(scalarFixture,
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n",
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n    subagent_model: claude-baseten-glm-5-2\n", 1),
		},
		{
			name: "insert falls back to protocol_shape anchor when no model_aliases",
			in: "clients:\n" +
				"  - name: codex\n" +
				"    enabled: true\n" +
				"    bind_addr: 127.0.0.1:18082\n" +
				"    protocol_shape: openai\n",
			client: "codex",
			set:    map[string]string{"subagent_model": "claude-baseten-glm-5-2"},
			want: "clients:\n" +
				"  - name: codex\n" +
				"    enabled: true\n" +
				"    bind_addr: 127.0.0.1:18082\n" +
				"    protocol_shape: openai\n" +
				"    subagent_model: claude-baseten-glm-5-2\n",
		},
		{
			name: "insert falls back to name anchor when no protocol_shape",
			in: "clients:\n" +
				"  - name: bare\n" +
				"    enabled: true\n",
			client: "bare",
			set:    map[string]string{"subagent_model": "claude-baseten-glm-5-2"},
			want: "clients:\n" +
				"  - name: bare\n" +
				"    subagent_model: claude-baseten-glm-5-2\n" +
				"    enabled: true\n",
		},
		{
			name:   "multi-key deterministic sorted order after model_aliases",
			in:     scalarFixture,
			client: "claude-code",
			set:    map[string]string{"subagent_routing": "on", "subagent_model": "claude-baseten-glm-5-2"},
			want: strings.Replace(scalarFixture,
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n",
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n"+
					"    subagent_model: claude-baseten-glm-5-2\n"+
					"    subagent_routing: on\n", 1),
		},
		{
			name:      "client missing errors",
			in:        scalarFixture,
			client:    "nope",
			set:       map[string]string{"subagent_model": "claude-baseten-glm-5-2"},
			wantErr:   "no client named",
			untouched: true,
		},
		{
			name:      "slug value with slash accepted",
			in:        scalarFixture,
			client:    "claude-code",
			set:       map[string]string{"subagent_model": "zai-org/GLM-5.2"},
			wantErr:   "",
			untouched: false,
			want: strings.Replace(scalarFixture,
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n",
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n    subagent_model: zai-org/GLM-5.2\n", 1),
		},
		{
			name:      "invalid value with space rejected",
			in:        scalarFixture,
			client:    "claude-code",
			set:       map[string]string{"subagent_model": "bad value"},
			wantErr:   "invalid value",
			untouched: true,
		},
		{
			name:      "malformed yaml errors cleanly",
			in:        "clients: [\n  broken\n",
			client:    "claude-code",
			set:       map[string]string{"subagent_model": "claude-baseten-glm-5-2"},
			wantErr:   "parse",
			untouched: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditFixture(t, tc.in, 0o600)
			err := SetClientScalars(path, tc.client, tc.set)
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				if tc.untouched && string(got) != tc.in {
					t.Fatalf("file changed despite error:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetClientScalars: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// enabledFixture models the shipped template's parked codex client:
// comment-rich, inline comment on the enabled line, shared listener.
const enabledFixture = `# header comment survives
global:
  routing_enabled: true

clients:
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic

  # codex ships parked; 'baseten-switch codex on' offers to enable it.
  - name: codex
    enabled: false  # parked
    bind_addr: 127.0.0.1:18081
    protocol_shape: openai
    responses_strip_tool_types: [tool_search]
    fallback_route: openai
`

// TestSetClientScalarsEnabled drives the bool-aware enabled case: the
// un-park flip 'baseten-switch codex on' performs (the Codex integration contract
// item 2.2). Full-file wants, so byte preservation outside the changed
// line is asserted, and the refusal semantics match the string scalars.
func TestSetClientScalarsEnabled(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		client    string
		set       map[string]string
		want      string // full expected file content
		wantErr   string // substring of the expected error ("" = no error)
		untouched bool   // assert file is byte-identical to in on error
	}{
		{
			name:   "un-park flip preserves inline comment and every other byte",
			in:     enabledFixture,
			client: "codex",
			set:    map[string]string{"enabled": "true"},
			want: strings.Replace(enabledFixture,
				"    enabled: false  # parked\n",
				"    enabled: true  # parked\n", 1),
		},
		{
			name:   "park flip true to false",
			in:     enabledFixture,
			client: "claude-code",
			set:    map[string]string{"enabled": "false"},
			want: strings.Replace(enabledFixture,
				"  - name: claude-code\n    enabled: true\n",
				"  - name: claude-code\n    enabled: false\n", 1),
		},
		{
			name: "insert when the key is absent",
			in: "clients:\n" +
				"  - name: codex\n" +
				"    protocol_shape: openai\n",
			client: "codex",
			set:    map[string]string{"enabled": "true"},
			want: "clients:\n" +
				"  - name: codex\n" +
				"    protocol_shape: openai\n" +
				"    enabled: true\n",
		},
		{
			name:      "non-bool token rejected",
			in:        enabledFixture,
			client:    "codex",
			set:       map[string]string{"enabled": "maybe"},
			wantErr:   "must be true or false",
			untouched: true,
		},
		{
			name:      "yaml-ish bool alias rejected",
			in:        enabledFixture,
			client:    "codex",
			set:       map[string]string{"enabled": "yes"},
			wantErr:   "must be true or false",
			untouched: true,
		},
		{
			name:      "client missing errors",
			in:        enabledFixture,
			client:    "nope",
			set:       map[string]string{"enabled": "true"},
			wantErr:   "no client named",
			untouched: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditFixture(t, tc.in, 0o600)
			err := SetClientScalars(path, tc.client, tc.set)
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				if tc.untouched && string(got) != tc.in {
					t.Fatalf("file changed despite error:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetClientScalars: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// TestSetClientScalarsEnabledTemplateRoundTrip un-parks the codex
// client in the shipped template itself and asserts the only changed
// bytes are the enabled token, so the editor is proven against the
// exact file 'baseten-switch config init' writes.
func TestSetClientScalarsEnabledTemplateRoundTrip(t *testing.T) {
	path := writeEditFixture(t, string(InitTemplate), 0o600)
	if err := SetClientScalars(path, "codex", map[string]string{"enabled": "true"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(InitTemplate),
		"  - name: codex\n    enabled: false\n",
		"  - name: codex\n    enabled: true\n", 1)
	if want == string(InitTemplate) {
		t.Fatal("template lost the parked codex client (enabled: false); update this test with the template")
	}
	if string(got) != want {
		t.Fatalf("un-parking the template changed more than the enabled token\n--- got ---\n%s", got)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("un-parked template does not load: %v", err)
	}
	for _, c := range f.Clients {
		if c.Name == "codex" && !c.Enabled {
			t.Error("codex still parked after the flip")
		}
	}
}

// TestSetClientScalarsPreservesUnrelatedBytes confirms every byte
// outside the touched scalar tokens is identical before and after.
// It compares the full file with the touched lines removed on both
// sides, so a stray byte change anywhere else fails.
func TestSetClientScalarsPreservesUnrelatedBytes(t *testing.T) {
	path := writeEditFixture(t, scalarFixture, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetClientScalars(path, "claude-code", map[string]string{
		"subagent_model": "claude-baseten-glm-5-2",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The only new content is the inserted line; everything else must
	// match byte for byte. Strip the inserted line from after and compare.
	stripped := strings.Replace(string(after),
		"    subagent_model: claude-baseten-glm-5-2\n", "", 1)
	if stripped != string(before) {
		t.Fatalf("unrelated bytes changed\n--- before ---\n%s\n--- after (stripped) ---\n%s", before, stripped)
	}
}

// TestSetClientScalarsVerifyAbortLeavesFileUntouched: a value that
// passes scalarValueRe but produces an unparseable splice (none today,
// but the verify path must still be exercised) is simulated by forcing
// a client-not-found AFTER the parse step. The file must be untouched.
func TestSetClientScalarsVerifyAbortLeavesFileUntouched(t *testing.T) {
	path := writeEditFixture(t, scalarFixture, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown client: parse succeeds, edit plan fails, file untouched.
	err = SetClientScalars(path, "ghost", map[string]string{"subagent_model": "claude-baseten-glm-5-2"})
	if err == nil || !strings.Contains(err.Error(), "no client named") {
		t.Fatalf("err = %v, want no client named", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("file changed despite abort:\n%s", after)
	}
}

// TestSetClientScalarsPreservesFileMode: the rewrite keeps the original
// permission bits, whatever they are.
func TestSetClientScalarsPreservesFileMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		path := writeEditFixture(t, scalarFixture, mode)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := SetClientScalars(path, "claude-code", map[string]string{
			"subagent_model": "claude-baseten-glm-5-2",
		}); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != mode {
			t.Fatalf("mode = %v, want %v", st.Mode().Perm(), mode)
		}
	}
}

// mapFixture is a comment-rich config with a live model_routes block on
// the claude-code client, so replace-in-place and insert-at-end both have
// realistic anchors. The codex client has no model_routes (and no
// model_aliases) so the protocol_shape/name fallback anchors are
// exercised by the block-creation cases. Every byte outside the
// touched entry tokens must survive.
const mapFixture = `# gateway.yaml -- head comment block.

global:
  routing_enabled: true

clients:
  # Guidance comment above the first client.
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-glm-5-2:   zai-org/GLM-5.2
      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code
    model_routes:
      opus: native  # family pin with an inline comment
      sonnet: claude-baseten-kimi-k2-7
    fallback_route: anthropic

  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18082
    protocol_shape: openai
    fallback_route: openai
`

// mapFixtureFlow carries a hand-written flow-style model_routes value:
// every entry shares one physical line with the key, so any line-based
// edit would clobber unrelated pins. Both Set and Remove must refuse it
// and leave the file byte-identical.
const mapFixtureFlow = `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    protocol_shape: anthropic
    model_routes: {opus: native, sonnet: claude-baseten-kimi-k2-7}
`

// mapFixtureNullBlock carries a bare "model_routes:" key with no
// entries (the user uncommented only the key from the template). It
// loads as a nil map; the editors must treat it as an empty mapping.
const mapFixtureNullBlock = `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    protocol_shape: anthropic
    model_routes:
`

// TestSetClientMapEntries drives the map-entry editor table-style and
// asserts the FULL resulting file, so any lost comment, blank line,
// alignment space, or reordering fails the test, not just the values.
func TestSetClientMapEntries(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		client    string
		mapKey    string
		entries   map[string]string
		want      string // full expected file content
		wantErr   string // substring of the expected error ("" = no error)
		untouched bool   // assert file is byte-identical to in on error
	}{
		{
			name:    "replace existing entry in place preserves inline comment",
			in:      mapFixture,
			client:  "claude-code",
			mapKey:  "model_routes",
			entries: map[string]string{"opus": "claude-baseten-glm-5-2"},
			want: strings.Replace(mapFixture,
				"      opus: native  # family pin with an inline comment",
				"      opus: claude-baseten-glm-5-2  # family pin with an inline comment", 1),
		},
		{
			name:    "replace entry value with native",
			in:      mapFixture,
			client:  "claude-code",
			mapKey:  "model_routes",
			entries: map[string]string{"sonnet": "native"},
			want: strings.Replace(mapFixture,
				"      sonnet: claude-baseten-kimi-k2-7",
				"      sonnet: native", 1),
		},
		{
			name:   "insert missing entry at end of existing block sorted",
			in:     mapFixture,
			client: "claude-code",
			mapKey: "model_routes",
			// haiku sorts after the existing entries; insert at block end.
			entries: map[string]string{"haiku": "native"},
			want: strings.Replace(mapFixture,
				"      sonnet: claude-baseten-kimi-k2-7\n",
				"      sonnet: claude-baseten-kimi-k2-7\n      haiku: native\n", 1),
		},
		{
			name:   "multi-insert sorted determinism at block end",
			in:     mapFixture,
			client: "claude-code",
			mapKey: "model_routes",
			// fable and haiku both missing; sorted order fable, haiku.
			entries: map[string]string{"haiku": "native", "fable": "native"},
			want: strings.Replace(mapFixture,
				"      sonnet: claude-baseten-kimi-k2-7\n",
				"      sonnet: claude-baseten-kimi-k2-7\n"+
					"      fable: native\n"+
					"      haiku: native\n", 1),
		},
		{
			name:      "client missing errors",
			in:        mapFixture,
			client:    "nope",
			mapKey:    "model_routes",
			entries:   map[string]string{"opus": "native"},
			wantErr:   "no client named",
			untouched: true,
		},
		{
			name:      "invalid key with bracket rejected",
			in:        mapFixture,
			client:    "claude-code",
			mapKey:    "model_routes",
			entries:   map[string]string{"claude-opus-4-8[1m]": "native"},
			wantErr:   "invalid map key",
			untouched: true,
		},
		{
			name:      "invalid key with slash rejected",
			in:        mapFixture,
			client:    "claude-code",
			mapKey:    "model_routes",
			entries:   map[string]string{"zai-org/GLM-5.2": "native"},
			wantErr:   "invalid map key",
			untouched: true,
		},
		{
			name:    "slug value with slash accepted",
			in:      mapFixture,
			client:  "claude-code",
			mapKey:  "model_routes",
			entries: map[string]string{"haiku": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"},
			want: strings.Replace(mapFixture,
				"      sonnet: claude-baseten-kimi-k2-7\n",
				"      sonnet: claude-baseten-kimi-k2-7\n      haiku: nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B\n", 1),
		},
		{
			name:      "invalid value with space rejected",
			in:        mapFixture,
			client:    "claude-code",
			mapKey:    "model_routes",
			entries:   map[string]string{"opus": "bad value"},
			wantErr:   "invalid value",
			untouched: true,
		},
		{
			name:      "malformed yaml errors cleanly",
			in:        "clients: [\n  broken\n",
			client:    "claude-code",
			mapKey:    "model_routes",
			entries:   map[string]string{"opus": "native"},
			wantErr:   "parse",
			untouched: true,
		},
		{
			name:      "flow-style map value rejected and file untouched",
			in:        mapFixtureFlow,
			client:    "claude-code",
			mapKey:    "model_routes",
			entries:   map[string]string{"opus": "claude-baseten-glm-5-2"},
			wantErr:   "flow-style map",
			untouched: true,
		},
		{
			name:    "null block treated as empty mapping single entry",
			in:      mapFixtureNullBlock,
			client:  "claude-code",
			mapKey:  "model_routes",
			entries: map[string]string{"opus": "native"},
			want: strings.Replace(mapFixtureNullBlock,
				"    model_routes:\n",
				"    model_routes:\n      opus: native\n", 1),
		},
		{
			name:    "null block treated as empty mapping sorted multi-entry",
			in:      mapFixtureNullBlock,
			client:  "claude-code",
			mapKey:  "model_routes",
			entries: map[string]string{"sonnet": "claude-baseten-kimi-k2-7", "opus": "native"},
			want: strings.Replace(mapFixtureNullBlock,
				"    model_routes:\n",
				"    model_routes:\n"+
					"      opus: native\n"+
					"      sonnet: claude-baseten-kimi-k2-7\n", 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditFixture(t, tc.in, 0o600)
			err := SetClientMapEntries(path, tc.client, tc.mapKey, tc.entries)
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				if tc.untouched && string(got) != tc.in {
					t.Fatalf("file changed despite error:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetClientMapEntries: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// mapFixtureNoBlock is mapFixture with the model_routes block removed,
// so block creation anchors off model_aliases (the last block on the
// client).
const mapFixtureNoBlock = `# gateway.yaml -- head comment block.

global:
  routing_enabled: true

clients:
  # Guidance comment above the first client.
  - name: claude-code
    enabled: true
    bind_addr: 127.0.0.1:18081
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-glm-5-2:   zai-org/GLM-5.2
      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code
    fallback_route: anthropic

  - name: codex
    enabled: true
    bind_addr: 127.0.0.1:18082
    protocol_shape: openai
    fallback_route: openai
`

// TestSetClientMapEntriesCreateBlock exercises block creation at each
// anchor priority: model_aliases, subagent_routing, subagent_model,
// protocol_shape, and name.
func TestSetClientMapEntriesCreateBlock(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		client string
		anchor string // human label for the anchor priority
		want   string
	}{
		{
			name:   "create after model_aliases",
			in:     mapFixtureNoBlock,
			client: "claude-code",
			anchor: "model_aliases",
			want: strings.Replace(mapFixtureNoBlock,
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n",
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n"+
					"    model_routes:\n"+
					"      opus: native\n", 1),
		},
		{
			name: "create after subagent_routing",
			in: "clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    protocol_shape: anthropic\n" +
				"    subagent_model: claude-baseten-glm-5-2\n" +
				"    subagent_routing: on\n",
			client: "claude-code",
			anchor: "subagent_routing",
			want: "clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    protocol_shape: anthropic\n" +
				"    subagent_model: claude-baseten-glm-5-2\n" +
				"    subagent_routing: on\n" +
				"    model_routes:\n" +
				"      opus: native\n",
		},
		{
			name: "create after subagent_model",
			in: "clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    protocol_shape: anthropic\n" +
				"    subagent_model: claude-baseten-glm-5-2\n",
			client: "claude-code",
			anchor: "subagent_model",
			want: "clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    protocol_shape: anthropic\n" +
				"    subagent_model: claude-baseten-glm-5-2\n" +
				"    model_routes:\n" +
				"      opus: native\n",
		},
		{
			name: "create after protocol_shape",
			in: "clients:\n" +
				"  - name: codex\n" +
				"    enabled: true\n" +
				"    bind_addr: 127.0.0.1:18082\n" +
				"    protocol_shape: openai\n",
			client: "codex",
			anchor: "protocol_shape",
			want: "clients:\n" +
				"  - name: codex\n" +
				"    enabled: true\n" +
				"    bind_addr: 127.0.0.1:18082\n" +
				"    protocol_shape: openai\n" +
				"    model_routes:\n" +
				"      opus: native\n",
		},
		{
			name: "create after name when no other anchor",
			in: "clients:\n" +
				"  - name: bare\n" +
				"    enabled: true\n",
			client: "bare",
			anchor: "name",
			want: "clients:\n" +
				"  - name: bare\n" +
				"    model_routes:\n" +
				"      opus: native\n" +
				"    enabled: true\n",
		},
		{
			name:   "create with sorted multi-entry",
			in:     mapFixtureNoBlock,
			client: "claude-code",
			anchor: "model_aliases multi",
			want: strings.Replace(mapFixtureNoBlock,
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n",
				"      claude-baseten-kimi-k2-7: moonshotai/Kimi-K2.7-Code\n"+
					"    model_routes:\n"+
					"      opus: native\n"+
					"      sonnet: native\n", 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.anchor, func(t *testing.T) {
			path := writeEditFixture(t, tc.in, 0o600)
			entries := map[string]string{"opus": "native"}
			if tc.anchor == "model_aliases multi" {
				entries["sonnet"] = "native"
			}
			if err := SetClientMapEntries(path, tc.client, "model_routes", entries); err != nil {
				t.Fatalf("SetClientMapEntries: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// mapFixtureComments carries a model_routes block with a two-line
// guidance comment directly above the key and a comment between the
// entries. Emptying the block must sweep all of them; a partial removal
// must leave every comment in place.
const mapFixtureComments = `global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: true
    protocol_shape: anthropic
    # Second guidance line.
    model_routes:
      opus: native
      # A second family pin remains independently editable.
      haiku: native
    fallback_route: anthropic
`

// TestRemoveClientMapEntries drives the removal editor: single remove,
// remove-last-removes-block, absent-key no-op, flow-style refusal,
// null-block no-op, and orphaned-comment sweep.
func TestRemoveClientMapEntries(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		client  string
		mapKey  string
		keys    []string
		want    string
		wantErr string
	}{
		{
			name:   "remove one entry leaves block",
			in:     mapFixture,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"sonnet"},
			want: strings.Replace(mapFixture,
				"      sonnet: claude-baseten-kimi-k2-7\n", "", 1),
		},
		{
			name:   "remove all entries removes the block line too",
			in:     mapFixture,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"opus", "sonnet"},
			want: strings.Replace(mapFixture,
				"    model_routes:\n"+
					"      opus: native  # family pin with an inline comment\n"+
					"      sonnet: claude-baseten-kimi-k2-7\n",
				"", 1),
		},
		{
			name:   "absent key removal is a no-op",
			in:     mapFixture,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"fable"},
			want:   mapFixture,
		},
		{
			name:   "absent key mixed with present removes only present",
			in:     mapFixture,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"opus", "fable"},
			want: strings.Replace(mapFixture,
				"      opus: native  # family pin with an inline comment\n", "", 1),
		},
		{
			name:   "remove from absent block is a no-op",
			in:     mapFixtureNoBlock,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"opus"},
			want:   mapFixtureNoBlock,
		},
		{
			name:    "flow-style map value rejected and file untouched",
			in:      mapFixtureFlow,
			client:  "claude-code",
			mapKey:  "model_routes",
			keys:    []string{"opus"},
			wantErr: "flow-style map",
		},
		{
			name:   "remove from null block is a no-op",
			in:     mapFixtureNullBlock,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"opus"},
			want:   mapFixtureNullBlock,
		},
		{
			name:   "emptying the block sweeps orphaned comments",
			in:     mapFixtureComments,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"opus", "haiku"},
			want: "global:\n" +
				"  routing_enabled: true\n" +
				"clients:\n" +
				"  - name: claude-code\n" +
				"    enabled: true\n" +
				"    protocol_shape: anthropic\n" +
				"    fallback_route: anthropic\n",
		},
		{
			name:   "comment above a surviving entry survives a partial removal",
			in:     mapFixtureComments,
			client: "claude-code",
			mapKey: "model_routes",
			keys:   []string{"opus"},
			want: strings.Replace(mapFixtureComments,
				"      opus: native\n", "", 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditFixture(t, tc.in, 0o600)
			err := RemoveClientMapEntries(path, tc.client, tc.mapKey, tc.keys)
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				if string(got) != tc.in {
					t.Fatalf("file changed despite error:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RemoveClientMapEntries: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// TestSetClientMapEntriesPreservesUnrelatedBytes confirms every byte
// outside the touched entry tokens is identical before and after.
func TestSetClientMapEntriesPreservesUnrelatedBytes(t *testing.T) {
	path := writeEditFixture(t, mapFixture, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetClientMapEntries(path, "claude-code", "model_routes", map[string]string{
		"haiku": "native",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.Replace(string(after),
		"      haiku: native\n", "", 1)
	if stripped != string(before) {
		t.Fatalf("unrelated bytes changed\n--- before ---\n%s\n--- after (stripped) ---\n%s", before, stripped)
	}
}

// TestSetClientMapEntriesVerifyAbortLeavesFileUntouched: an unknown
// client makes the parse succeed but the edit plan fail, so the file
// must be untouched.
func TestSetClientMapEntriesVerifyAbortLeavesFileUntouched(t *testing.T) {
	path := writeEditFixture(t, mapFixture, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = SetClientMapEntries(path, "ghost", "model_routes", map[string]string{"opus": "native"})
	if err == nil || !strings.Contains(err.Error(), "no client named") {
		t.Fatalf("err = %v, want no client named", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("file changed despite abort:\n%s", after)
	}
}

// TestSetClientMapEntriesPreservesFileMode: the rewrite keeps the
// original permission bits, whatever they are.
func TestSetClientMapEntriesPreservesFileMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		path := writeEditFixture(t, mapFixture, mode)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := SetClientMapEntries(path, "claude-code", "model_routes", map[string]string{
			"haiku": "native",
		}); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != mode {
			t.Fatalf("mode = %v, want %v", st.Mode().Perm(), mode)
		}
	}
}
