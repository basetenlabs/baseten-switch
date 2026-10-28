package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Small explicit limits use the compatibility floor on Desktop requests.
func TestDesktopMaxTokensFloorReachesBaseten(t *testing.T) {
	model := desktopModelID("claude-baseten-alpha", "example/Alpha")
	for _, test := range []struct {
		name     string
		field    string
		want     any // nil = field absent upstream
		upstream string
		stream   bool
	}{
		{name: "one", field: `,"max_tokens":1`, want: 8.0},
		{name: "zero", field: `,"max_tokens":0`, want: 8.0},
		{name: "two", field: `,"max_tokens":2`, want: 8.0},
		{name: "below floor", field: `,"max_tokens":7`, want: 8.0},
		{name: "at floor", field: `,"max_tokens":8`, want: 8.0},
		{name: "above floor", field: `,"max_tokens":9`, want: 9.0},
		{name: "large", field: `,"max_tokens":32000`, want: 32000.0},
		{name: "absent", want: nil},
		{name: "streaming", field: `,"max_tokens":1`, want: 8.0, stream: true},
		{name: "translated openai upstream", field: `,"max_tokens":1`, want: 8.0, upstream: "openai"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got map[string]any
			baseten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if test.upstream == "openai" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"example/Alpha","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
					return
				}
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer baseten.Close()
			g, cl := noNativeTestGateway(t, baseten.URL, "http://example.invalid")
			cl.cfg.UpstreamShape = test.upstream
			stream := ""
			if test.stream {
				stream = `,"stream":true`
			}
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]` + test.field + stream + `}`
			w := httptest.NewRecorder()
			g.handleClient(cl, w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if w.Code != 200 {
				t.Fatalf("status=%d: %s", w.Code, w.Body.String())
			}
			if v, ok := got["max_tokens"]; v != test.want || (test.want == nil && ok) {
				t.Fatalf("upstream max_tokens=%v (present=%t), want %v", v, ok, test.want)
			}
		})
	}
}

// The floor applies to every Baseten-bound attempt regardless of client
// or wire shape, and never to native provider routes.
func TestBuildAttemptMaxTokensFloor(t *testing.T) {
	for _, test := range []struct {
		name, route, kind, body string
		cfg                     func(*testing.T) resolvedClientConfig
		field                   string
		want                    any
	}{
		{name: "claude-code messages", route: "baseten", kind: "messages", body: `{"model":"claude-opus-4-8","max_tokens":1}`, cfg: resolvedAnthropicBaseten, field: "max_tokens", want: 8.0},
		{name: "codex chat max_tokens", route: "baseten", kind: "chat", body: `{"model":"m","max_tokens":1}`, cfg: openAIBaseten, field: "max_tokens", want: 8.0},
		{name: "codex chat max_completion_tokens", route: "baseten", kind: "chat", body: `{"model":"m","max_completion_tokens":1}`, cfg: openAIBaseten, field: "max_completion_tokens", want: 8.0},
		{name: "codex responses max_output_tokens", route: "baseten", kind: "responses", body: `{"model":"m","max_output_tokens":1}`, cfg: openAIBaseten, field: "max_output_tokens", want: 8.0},
		{name: "null max_tokens", route: "baseten", kind: "chat", body: `{"model":"m","max_tokens":null}`, cfg: openAIBaseten, field: "max_tokens", want: nil},
		{name: "null max_completion_tokens", route: "baseten", kind: "chat", body: `{"model":"m","max_completion_tokens":null}`, cfg: openAIBaseten, field: "max_completion_tokens", want: nil},
		{name: "null max_output_tokens", route: "baseten", kind: "responses", body: `{"model":"m","max_output_tokens":null}`, cfg: openAIBaseten, field: "max_output_tokens", want: nil},
		{name: "large untouched", route: "baseten", kind: "chat", body: `{"model":"m","max_completion_tokens":4096}`, cfg: openAIBaseten, field: "max_completion_tokens", want: 4096.0},
		{name: "native anthropic fallback untouched", route: "anthropic", kind: "messages", body: `{"model":"claude-opus-4-8","max_tokens":1}`, cfg: resolvedAnthropicBaseten, field: "max_tokens", want: 1.0},
		{name: "native openai untouched", route: "openai", kind: "responses", body: `{"model":"m","max_output_tokens":1}`, cfg: openAIBaseten, field: "max_output_tokens", want: 1.0},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, _ := noNativeTestGateway(t, "http://example.invalid", "http://example.invalid")
			cl := &clientListener{cfg: test.cfg(t)}
			at, err := g.buildAttempt(cl, httptest.NewRequest(http.MethodPost, "/", nil), []byte(test.body), test.route, test.kind)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(at.res.NewBody, &got); err != nil {
				t.Fatal(err)
			}
			if got[test.field] != test.want {
				t.Fatalf("%s=%v, want %v (body %s)", test.field, got[test.field], test.want, at.res.NewBody)
			}
		})
	}
}

func openAIBaseten(t *testing.T) resolvedClientConfig {
	return resolvedOpenAIBaseten(t, "codex", "baseten")
}

func TestRaiseMaxTokensFloorKeepsBytesWhenUnchanged(t *testing.T) {
	for _, body := range []string{`{"model":"m"}`, `{"model":"m","max_tokens":8}`, `{"max_tokens":null}`, `{"max_completion_tokens":null}`, `{"max_output_tokens":null}`, `{"max_tokens":"x"}`, `{"model":`} {
		if got := string(raiseMaxTokensFloor([]byte(body))); got != body {
			t.Fatalf("body %s rewritten to %s", body, got)
		}
	}
}
