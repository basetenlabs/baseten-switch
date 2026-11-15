package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

type failedDiscoveryWriter struct{ *httptest.ResponseRecorder }

func (w failedDiscoveryWriter) Write(p []byte) (int, error) { return 0, errors.New("closed") }

func TestDesktopDiscoveryEvidenceRequiresSuccessfulCurrentList(t *testing.T) {
	g, cl := noNativeTestGateway(t, "http://example.invalid", "http://example.invalid")
	g.clients = map[string]*clientListener{config.ClaudeDesktopClientName: cl}
	g.activeConfigFile = noNativePolicyFile()
	for _, target := range []string{"/unknown", "/v1/models?limit=bad", "/v1/models?limit=1", "/v1/models?after_id=" + desktopModelID("claude-baseten-beta", "example/Beta")} {
		g.handleClient(cl, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
		if g.activeRoutingState().desktopModelDiscovery != nil {
			t.Fatalf("%s recorded discovery", target)
		}
	}
	g.aliasModelsGet(cl, httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/v1/models", nil))
	if g.activeRoutingState().desktopModelDiscovery != nil {
		t.Fatal("HEAD recorded discovery")
	}
	g.aliasModelsGet(cl, failedDiscoveryWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if g.activeRoutingState().desktopModelDiscovery != nil {
		t.Fatal("failed write recorded discovery")
	}
	g.handleClient(cl, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	evidence := g.activeRoutingState().desktopModelDiscovery
	if evidence == nil || evidence.ModelCount != 2 || evidence.Revision != config.DesktopModelRevision(cl.cfg.SelectedModelAliases, cl.cfg.ModelAliases) {
		t.Fatalf("missing successful list evidence: %#v", evidence)
	}
	if _, err := time.Parse(time.RFC3339Nano, evidence.LastServedAt); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	g.adminStatus(w, httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil))
	var status struct {
		Discovery *desktopModelDiscovery `json:"claude_desktop_model_discovery"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Discovery == nil || *status.Discovery != *evidence {
		t.Fatalf("admin evidence missing: %s (%v)", w.Body.String(), err)
	}
	file := noNativePolicyFile()
	raw, err := config.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	g.activateConfigSnapshot(file, raw)
	if g.activeRoutingState().desktopModelDiscovery != nil {
		t.Fatal("activation retained old discovery")
	}
	g.recordDesktopModelDiscovery(cl, 0, 2)
	if g.activeRoutingState().desktopModelDiscovery != nil {
		t.Fatal("in-flight previous generation recorded discovery")
	}
	old := *cl
	g.aliasModelsGet(&old, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if g.activeRoutingState().desktopModelDiscovery != nil {
		t.Fatal("retired listener recorded discovery")
	}
	cl.cfg.Name = "claude-code"
	g.aliasModelsGet(cl, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if g.activeRoutingState().desktopModelDiscovery != nil {
		t.Fatal("Code discovery recorded Desktop evidence")
	}
}

func TestDesktopModelIDStableIdentity(t *testing.T) {
	const want = "claude-baseten-model-92579695905743316876903764038803629199238914049400643556788923899124521378697"
	if got := desktopModelID("claude-baseten-alpha", "example/Alpha"); got != want {
		t.Fatalf("stable ID changed: %q", got)
	}
	seen := map[string]bool{}
	for _, pair := range [][2]string{
		{"claude-baseten-deepseek", "example/DeepSeek"},
		{"claude-baseten-glm", "example/GLM"},
		{"claude-baseten-qwen", "example/Qwen"},
		{"claude-baseten-deepseek", "example/Replacement"},
		{"claude-baseten-another", "example/DeepSeek"},
		{"a", "bc"}, {"ab", "c"},
	} {
		id := desktopModelID(pair[0], pair[1])
		if len(id) > 255 || !strings.HasPrefix(id, desktopModelIDPrefix) {
			t.Fatalf("invalid discovery ID %q", id)
		}
		for _, digit := range strings.TrimPrefix(id, desktopModelIDPrefix) {
			if digit < '0' || digit > '9' {
				t.Fatalf("non-numeric token in %q", id)
			}
		}
		if seen[id] {
			t.Fatalf("distinct identities share %q", id)
		}
		seen[id] = true
	}
}

func TestDesktopDiscoveryKeepsNamesAndStableOrder(t *testing.T) {
	g, cl := noNativeTestGateway(t, "http://example.invalid", "http://example.invalid")
	aliases := []string{"claude-baseten-deepseek", "claude-baseten-glm", "claude-baseten-qwen"}
	labels := []string{"DeepSeek", "GLM", "Qwen"}
	cl.cfg.ModelAliases = map[string]string{}
	for i, alias := range aliases {
		cl.cfg.ModelAliases[alias] = "example/" + labels[i]
	}
	cl.cfg.SelectedModelAliases = aliases
	discover := func() modelsList {
		t.Helper()
		w := httptest.NewRecorder()
		g.handleClient(cl, w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("discovery status=%d", w.Code)
		}
		var list modelsList
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		return list
	}
	first := discover()
	if len(first.Data) != len(aliases) {
		t.Fatalf("model count=%d", len(first.Data))
	}
	for i, entry := range first.Data {
		if entry.DisplayName != labels[i]+" via Baseten" {
			t.Fatalf("display name=%q", entry.DisplayName)
		}
	}
	file := noNativePolicyFile()
	file.Clients[0].ModelAliases = cl.cfg.ModelAliases
	file.Clients[0].ModelPicker.Models = nil
	for _, alias := range aliases {
		file.Clients[0].ModelPicker.Models = append(file.Clients[0].ModelPicker.Models, config.ModelPickerModel{Alias: alias})
	}
	resolved, err := resolveFromFile(file)
	if err != nil {
		t.Fatal(err)
	}
	cl.cfg = resolved[0]
	if second := discover(); !reflect.DeepEqual(first, second) {
		t.Fatal("IDs changed on client rebuild")
	}
	cl.cfg.SelectedModelAliases = []string{aliases[2], aliases[0], aliases[1]}
	reordered := discover()
	for i, before := range []int{2, 0, 1} {
		if reordered.Data[i] != first.Data[before] {
			t.Fatal("reorder changed a model identity or label")
		}
	}
	cl.cfg.Name = "claude-code"
	cl.cfg.NativeDisabled = false
	for _, entry := range discover().Data {
		if strings.HasPrefix(entry.ID, desktopModelIDPrefix) {
			t.Fatal("Desktop IDs leaked into Code discovery")
		}
		if _, ok := cl.cfg.ModelAliases[entry.ID]; !ok {
			t.Fatalf("Code alias changed: %q", entry.ID)
		}
	}
}

func TestDesktopDisplayLabelsKeepIdentity(t *testing.T) {
	for slug, want := range map[string]string{"example/DeepSeek-V4.1-Flash-1017": "DeepSeek V4.1 Flash 1017 via Baseten", "example/GLM-5.2": "GLM 5.2 via Baseten"} {
		entry := aliasModelEntry("unchanged-id", slug)
		if entry["id"] != "unchanged-id" || entry["display_name"] != want {
			t.Fatalf("unexpected label: %#v", entry)
		}
	}
}
