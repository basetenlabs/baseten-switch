package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewAdminSecretsWritesOnlyExplicitPreviewEnv(t *testing.T) {
	base := t.TempDir()
	stableRoot := filepath.Join(base, "baseten-switch")
	previewRoot := filepath.Join(base, "baseten-switch-preview")
	if err := os.Mkdir(stableRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(previewRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stableEnv := filepath.Join(stableRoot, "env")
	previewEnv := filepath.Join(previewRoot, "env")
	if err := os.WriteFile(stableEnv, []byte("STABLE_ONLY=unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previewEnv, []byte("# preview\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_PRIVATE_RUNTIME", "1")
	t.Setenv("BASETEN_SWITCH_ENV_FILE", previewEnv)

	g := &Gateway{cfg: Config{
		ConfigPath: filepath.Join(previewRoot, "gateway.yaml"),
	}}
	req := httptest.NewRequest(
		http.MethodPut,
		"/v1/admin/secrets",
		strings.NewReader(`{"name":"PREVIEW_ONLY","value":"preview-secret"}`))
	rec := httptest.NewRecorder()
	g.adminSecrets(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	gotPreview, err := os.ReadFile(previewEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotPreview), "PREVIEW_ONLY=preview-secret") {
		t.Fatalf("Preview env was not updated: %q", gotPreview)
	}
	gotStable, err := os.ReadFile(stableEnv)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotStable) != "STABLE_ONLY=unchanged\n" {
		t.Fatalf("Stable env changed: %q", gotStable)
	}
}

func TestPreviewAdminSecretsRejectsStableOrSymlinkEnv(t *testing.T) {
	base := t.TempDir()
	stableRoot := filepath.Join(base, "baseten-switch")
	previewRoot := filepath.Join(base, "baseten-switch-preview")
	if err := os.Mkdir(stableRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(previewRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stableEnv := filepath.Join(stableRoot, "env")
	if err := os.WriteFile(stableEnv, []byte("STABLE_ONLY=unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previewEnv := filepath.Join(previewRoot, "env")
	if err := os.Symlink(stableEnv, previewEnv); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_PRIVATE_RUNTIME", "1")
	t.Setenv("BASETEN_SWITCH_ENV_FILE", previewEnv)

	g := &Gateway{cfg: Config{
		ConfigPath: filepath.Join(previewRoot, "gateway.yaml"),
	}}
	req := httptest.NewRequest(
		http.MethodPut,
		"/v1/admin/secrets",
		strings.NewReader(`{"name":"ESCAPE","value":"blocked"}`))
	rec := httptest.NewRecorder()
	g.adminSecrets(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(stableEnv)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "STABLE_ONLY=unchanged\n" {
		t.Fatalf("Stable env changed through symlink: %q", got)
	}
}
