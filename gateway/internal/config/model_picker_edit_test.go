package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const modelPickerEditFixture = `# outside-before
global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: false
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-alpha: example/Alpha
      claude-baseten-beta: example/Beta
    model_picker:
      enabled: true # enabled-comment
      models:
        # alpha-comment
        - alias: claude-baseten-alpha # alpha-inline
        # beta-comment
        - alias: claude-baseten-beta # beta-inline
    fallback_route: anthropic
# outside-after
`

func TestSetClientModelPickerReordersRowsWithComments(t *testing.T) {
	path := writeEditFixture(t, modelPickerEditFixture, 0o640)
	want := &ModelPicker{
		Enabled: false,
		Models: []ModelPickerModel{
			{Alias: "claude-baseten-beta"},
			{Alias: "claude-baseten-alpha"},
		},
	}
	if err := SetClientModelPicker(path, "claude-code", want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte("# outside-before\nglobal:\n")) ||
		!bytes.HasSuffix(got, []byte("    fallback_route: anthropic\n# outside-after\n")) {
		t.Fatalf("bytes outside model_picker changed:\n%s", got)
	}
	text := string(got)
	betaComment := strings.Index(text, "# beta-comment")
	betaRow := strings.Index(text, "alias: claude-baseten-beta")
	alphaComment := strings.Index(text, "# alpha-comment")
	alphaRow := strings.Index(text, "alias: claude-baseten-alpha")
	if betaComment < 0 || betaRow < betaComment || alphaComment < betaRow || alphaRow < alphaComment {
		t.Fatalf("row comments did not move with reordered rows:\n%s", got)
	}
	if !strings.Contains(text, "enabled: false # enabled-comment") {
		t.Fatalf("enabled inline comment was not preserved:\n%s", got)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoutingPolicy(loaded); err != nil {
		t.Fatal(err)
	}
	if !modelPickersEqual(loaded.Clients[0].ModelPicker, want) {
		t.Fatalf("model_picker = %#v, want %#v", loaded.Clients[0].ModelPicker, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}

	before := append([]byte(nil), got...)
	if err := SetClientModelPicker(path, "claude-code", want); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("unchanged picker was not byte-stable\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestSetClientModelPickerUnchangedIsByteStable(t *testing.T) {
	path := writeEditFixture(t, modelPickerEditFixture, 0o600)
	picker := &ModelPicker{
		Enabled: true,
		Models: []ModelPickerModel{
			{Alias: "claude-baseten-alpha"},
			{Alias: "claude-baseten-beta"},
		},
	}
	if err := SetClientModelPicker(path, "claude-code", picker); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(modelPickerEditFixture)) {
		t.Fatalf("unchanged picker was rewritten\n--- got ---\n%s", got)
	}
}

func TestSetClientModelPickerCreatesAndRemovesBlock(t *testing.T) {
	const fixture = `# keep-before
global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: false
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-alpha: example/Alpha
    fallback_route: anthropic
# keep-after
`
	path := writeEditFixture(t, fixture, 0o600)
	picker := &ModelPicker{Enabled: true, Models: []ModelPickerModel{{
		Alias: "claude-baseten-alpha",
	}}}
	if err := SetClientModelPicker(path, "claude-code", picker); err != nil {
		t.Fatal(err)
	}
	created, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(created), "model_picker:\n      enabled: true\n      models:\n        - alias: claude-baseten-alpha") {
		t.Fatalf("model_picker block not created after model_aliases:\n%s", created)
	}
	if err := SetClientModelPicker(path, "claude-code", nil); err != nil {
		t.Fatal(err)
	}
	removed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(removed, []byte(fixture)) {
		t.Fatalf("create/remove did not restore fixture byte-for-byte\n--- got ---\n%s\n--- want ---\n%s", removed, fixture)
	}
}

func TestSetClientModelPickerRejectsUnsafeStateWithoutWrite(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		picker  *ModelPicker
		want    string
	}{
		{
			name: "flow style picker",
			fixture: strings.Replace(modelPickerEditFixture,
				"    model_picker:\n      enabled: true # enabled-comment\n      models:\n        # alpha-comment\n        - alias: claude-baseten-alpha # alpha-inline\n        # beta-comment\n        - alias: claude-baseten-beta # beta-inline\n",
				"    model_picker: {enabled: true, models: [{alias: claude-baseten-alpha}]}\n", 1),
			picker: &ModelPicker{Enabled: true, Models: []ModelPickerModel{{Alias: "claude-baseten-alpha"}}},
			want:   "flow-style",
		},
		{
			name:    "alias not configured",
			fixture: modelPickerEditFixture,
			picker: &ModelPicker{Enabled: true, Models: []ModelPickerModel{{
				Alias: "claude-baseten-missing",
			}}},
			want: "missing from model_aliases",
		},
		{
			name:    "unsafe alias scalar",
			fixture: modelPickerEditFixture,
			picker: &ModelPicker{Enabled: true, Models: []ModelPickerModel{{
				Alias: "claude baseten alpha",
			}}},
			want: "invalid model picker alias",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditFixture(t, tc.fixture, 0o600)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = SetClientModelPicker(path, "claude-code", tc.picker)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetClientModelPicker() error = %v, want substring %q", err, tc.want)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("file changed on error\n--- before ---\n%s\n--- after ---\n%s", before, after)
			}
		})
	}
}

func TestSetClientModelAliasesAddsEntryWithoutRewritingExistingMap(t *testing.T) {
	const fixture = `# keep
global:
  routing_enabled: true
clients:
  - name: claude-code
    enabled: false
    protocol_shape: anthropic
    model_aliases:
      claude-baseten-alpha: example/Alpha # keep-inline
    fallback_route: anthropic
`
	path := writeEditFixture(t, fixture, 0o600)
	if err := SetClientModelAliases(path, "claude-code", map[string]string{
		"claude-baseten-beta": "example/Beta",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(
		fixture,
		"      claude-baseten-alpha: example/Alpha # keep-inline\n",
		"      claude-baseten-alpha: example/Alpha # keep-inline\n      claude-baseten-beta: example/Beta\n",
		1,
	)
	if string(got) != want {
		t.Fatalf("file mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Clients[0].ModelAliases["claude-baseten-beta"] != "example/Beta" {
		t.Fatalf("model_aliases = %v", loaded.Clients[0].ModelAliases)
	}
}

func TestSetClientModelAliasesRejectsUnsafeEntryWithoutWrite(t *testing.T) {
	path := writeEditFixture(t, modelPickerEditFixture, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = SetClientModelAliases(path, "claude-code", map[string]string{
		"claude baseten unsafe": "example/Beta",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid model alias") {
		t.Fatalf("SetClientModelAliases() error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("file changed on rejected alias")
	}
}
