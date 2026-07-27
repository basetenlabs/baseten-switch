package route

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, r := range Routes {
		if !Valid(r) {
			t.Fatalf("expected %q valid", r)
		}
	}
	for _, bad := range []string{"", "bas ten", "Baseten", "banana", "anthr"} {
		if Valid(bad) {
			t.Fatalf("expected %q invalid", bad)
		}
	}
}

func TestWriteRejectsInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad-route")
	if err := WriteTo(p, "banana"); err == nil {
		t.Fatal("expected error for invalid route")
	}
}

func TestWriteToPersistsExplicitAdminSelection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "route")
	if err := WriteTo(p, "anthropic"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "anthropic\n" {
		t.Fatalf("route file = %q, want %q", got, "anthropic\n")
	}
}
