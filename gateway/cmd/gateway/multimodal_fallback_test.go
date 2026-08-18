package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
	"github.com/basetenlabs/baseten-switch/gateway/internal/upstreamerror"
)

func TestHasIdentityContentEncoding(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		want     bool
	}{
		{name: "absent", want: true},
		{name: "identity", encoding: "identity", want: true},
		{name: "identity case and whitespace", encoding: " Identity ", want: true},
		{name: "gzip", encoding: "gzip", want: false},
		{name: "encoding chain", encoding: "identity, gzip", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			if test.encoding != "" {
				header.Set("Content-Encoding", test.encoding)
			}
			if got := hasIdentityContentEncoding(header); got != test.want {
				t.Fatalf("hasIdentityContentEncoding(%q) = %t, want %t", test.encoding, got, test.want)
			}
		})
	}
}

func TestImageTranslationErrorUsesNativeFallback(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			t.Error("Baseten must not receive an image the translator rejects")
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	var nativeBody []byte
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			nativeHits.Add(1)
			nativeBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"msg_native","type":"message","role":"assistant",` +
					`"content":[{"type":"text","text":"FALLBACK"}],` +
					`"model":"claude-opus-4-8",` +
					`"usage":{"input_tokens":2,"output_tokens":1}}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.UpstreamShape = "openai"
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"describe this"},
				{"type":"image","source":{
					"type":"base64",
					"media_type":"image/png",
					"data":"aW1hZ2U="
				}}
			]
		}]
	}`)
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "claude-code", "/v1/messages"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, responseBody)
	}
	if basetenHits.Load() != 0 {
		t.Fatalf("Baseten hits = %d, want 0", basetenHits.Load())
	}
	if nativeHits.Load() != 1 {
		t.Fatalf("native hits = %d, want 1", nativeHits.Load())
	}
	if !bytes.Equal(nativeBody, body) {
		t.Fatalf("native body changed\ngot:  %s\nwant: %s", nativeBody, body)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].Fallback.Trigger == nil ||
		*rows[0].Fallback.Trigger != fallbackTriggerImageUnsupported {
		got := ""
		if rows[0].Fallback.Trigger != nil {
			got = *rows[0].Fallback.Trigger
		}
		t.Fatalf(
			"fallback trigger = %q, want %q",
			got,
			fallbackTriggerImageUnsupported,
		)
	}
	if primary := rows[0].Primary; primary == nil ||
		primary.Provider != "baseten" ||
		primary.Model != reasoningNeutralBasetenModel ||
		primary.Attempted ||
		primary.Outcome != telemetry.PrimaryOutcomeImageInputUnsupported ||
		primary.Status != nil {
		t.Fatalf("preflight image primary = %+v", primary)
	}
}

func TestToolResultImageTranslationErrorUsesNativeFallback(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			t.Error("Baseten must not receive a tool-result image the translator rejects")
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	var nativeBody []byte
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			nativeHits.Add(1)
			nativeBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"msg_native","type":"message","role":"assistant",` +
					`"content":[{"type":"text","text":"FALLBACK"}],` +
					`"model":"claude-opus-4-8",` +
					`"usage":{"input_tokens":2,"output_tokens":1}}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.UpstreamShape = "openai"
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"tu_1",
				"content":[
					{"type":"text","text":"rendered output"},
					{"type":"image","source":{
						"type":"base64",
						"media_type":"image/png",
						"data":"aW1hZ2U="
					}}
				]
			}]
		}]
	}`)
	resp, responseBody := postMultimodalMessages(
		t,
		g,
		"claude-code",
		body,
	)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(responseBody), "FALLBACK") {
		t.Fatalf("response = %d %s, want native 200", resp.StatusCode, responseBody)
	}
	if basetenHits.Load() != 0 || nativeHits.Load() != 1 {
		t.Fatalf(
			"hits = Baseten %d native %d, want 0/1",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
	if !bytes.Equal(nativeBody, body) {
		t.Fatalf("native body changed\ngot:  %s\nwant: %s", nativeBody, body)
	}
}

func TestReactiveImage400FallbackMessages(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(
				w,
				`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`,
				upstreamerror.BasetenMultimodalUnsupportedMessage,
			)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"msg_native","type":"message","role":"assistant",` +
					`"content":[{"type":"text","text":"FALLBACK"}],` +
					`"model":"claude-opus-4-8",` +
					`"usage":{"input_tokens":2,"output_tokens":1}}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	imageBody := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image","source":{
				"type":"base64",
				"media_type":"image/png",
				"data":"aW1hZ2U="
			}}
		]}]
	}`)
	resp, responseBody := postMultimodalMessages(
		t,
		g,
		"claude-code",
		imageBody,
	)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(responseBody), "FALLBACK") {
		t.Fatalf("image response = %d %s, want native 200", resp.StatusCode, responseBody)
	}
	if basetenHits.Load() != 1 || nativeHits.Load() != 1 {
		t.Fatalf(
			"hits after image = Baseten %d native %d, want 1/1",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
	if g.fallbackActive("claude-code") {
		t.Fatal("reactive image fallback must not start the health cooldown")
	}

	rows := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)
	if rows[0].Fallback.Trigger == nil ||
		*rows[0].Fallback.Trigger != fallbackTriggerImageUnsupported {
		t.Fatalf(
			"fallback trigger = %v, want %q",
			rows[0].Fallback.Trigger,
			fallbackTriggerImageUnsupported,
		)
	}
	if primary := rows[0].Primary; primary == nil ||
		primary.Provider != "baseten" ||
		primary.Model != reasoningNeutralBasetenModel ||
		!primary.Attempted ||
		primary.Outcome != telemetry.PrimaryOutcomeImageInputUnsupported ||
		primary.Status == nil || *primary.Status != http.StatusBadRequest {
		t.Fatalf("reactive image primary = %+v", primary)
	}

	textBody := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{"role":"user","content":"text only"}]
	}`)
	resp, responseBody = postMultimodalMessages(
		t,
		g,
		"claude-code",
		textBody,
	)
	if resp.StatusCode != http.StatusBadRequest ||
		!bytes.Contains(
			responseBody,
			[]byte(upstreamerror.BasetenMultimodalUnsupportedMessage),
		) {
		t.Fatalf(
			"text response = %d %s, want original Baseten 400",
			resp.StatusCode,
			responseBody,
		)
	}
	if basetenHits.Load() != 2 || nativeHits.Load() != 1 {
		t.Fatalf(
			"hits after text = Baseten %d native %d, want 2/1",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
}

func TestReactiveImage400RequiresExactSignature(t *testing.T) {
	originalBody := []byte(
		`{"type":"error","error":{"type":"invalid_request_error",` +
			`"message":"invalid tools for this model"}}`,
	)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Upstream-Marker", "preserved")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(originalBody)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{
			"type":"image",
			"source":{"type":"url","url":"https://example.invalid/image.png"}
		}]}]
	}`)
	resp, responseBody := postMultimodalMessages(
		t,
		g,
		"claude-code",
		body,
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !bytes.Equal(responseBody, originalBody) {
		t.Fatalf(
			"nonmatching body changed\ngot:  %s\nwant: %s",
			responseBody,
			originalBody,
		)
	}
	if resp.Header.Get("X-Upstream-Marker") != "preserved" {
		t.Fatalf(
			"upstream marker = %q, want preserved",
			resp.Header.Get("X-Upstream-Marker"),
		)
	}
	if nativeHits.Load() != 0 {
		t.Fatalf("native hits = %d, want 0", nativeHits.Load())
	}
}

func TestReactiveImage400ClassifierReadHonorsTTFT(t *testing.T) {
	fullBody := fmt.Sprintf(
		`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`,
		upstreamerror.BasetenMultimodalUnsupportedMessage,
	)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fullBody)))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":`))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		},
	))
	defer baseten.Close()

	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"msg_native","type":"message","role":"assistant",` +
					`"content":[{"type":"text","text":"FALLBACK"}],` +
					`"model":"claude-opus-4-8",` +
					`"usage":{"input_tokens":2,"output_tokens":1}}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "anthropic"
	rc.TTFTTimeout = 100 * time.Millisecond
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{
			"type":"image",
			"source":{"type":"url","url":"https://example.invalid/image.png"}
		}]}]
	}`)
	started := time.Now()
	resp, responseBody := postMultimodalMessages(
		t,
		g,
		"claude-code",
		body,
	)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(responseBody), "FALLBACK") {
		t.Fatalf("response = %d %s, want native 200", resp.StatusCode, responseBody)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("classifier read took %s, want TTFT-bounded retry", elapsed)
	}

	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Fallback.Trigger == nil ||
		*row.Fallback.Trigger != fallbackTriggerTTFT {
		t.Fatalf(
			"fallback trigger = %v, want %q",
			row.Fallback.Trigger,
			fallbackTriggerTTFT,
		)
	}
}

func TestReactiveImage400UnknownLengthPassesThrough(t *testing.T) {
	originalBody := fmt.Sprintf(
		`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`,
		upstreamerror.BasetenMultimodalUnsupportedMessage,
	)
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(originalBody))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "anthropic"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{
			"type":"image",
			"source":{"type":"url","url":"https://example.invalid/image.png"}
		}]}]
	}`)
	started := time.Now()
	resp, responseBody := postMultimodalMessages(
		t,
		g,
		"claude-code",
		body,
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want original 400", resp.StatusCode)
	}
	if !bytes.Equal(responseBody, []byte(originalBody)) {
		t.Fatalf(
			"unknown-length body changed\ngot:  %q\nwant: %q",
			responseBody,
			originalBody,
		)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("pass-through took %s, want immediate relay", elapsed)
	}
	if nativeHits.Load() != 0 {
		t.Fatalf("native hits = %d, want 0 for unknown-length 400", nativeHits.Load())
	}
}

func TestReactiveImage400FallbackResponses(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(
				w,
				`{"message":%q,"type":"Bad Request","code":400}`,
				upstreamerror.BasetenMultimodalUnsupportedMessage,
			)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"resp_native","object":"response","status":"completed",` +
					`"model":"gpt-native","output":[]}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	cfg.OpenAIURL = native.URL
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "openai"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"gpt-native",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"describe this"},
			{"type":"input_image","image_url":"https://example.invalid/image.png"}
		]}]
	}`)
	resp, responseBody := postMultimodalResponses(t, g, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, responseBody)
	}
	if basetenHits.Load() != 1 || nativeHits.Load() != 1 {
		t.Fatalf(
			"hits = Baseten %d native %d, want 1/1",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
}

func TestReactiveImage400FallbackChatCompletions(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(
				w,
				`{"message":%q,"type":"Bad Request","code":400}`,
				upstreamerror.BasetenMultimodalUnsupportedMessage,
			)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"chat_native","object":"chat.completion",` +
					`"model":"gpt-native","choices":[{"index":0,` +
					`"message":{"role":"assistant","content":"FALLBACK"},` +
					`"finish_reason":"stop"}]}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	cfg.OpenAIURL = native.URL
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "openai"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"gpt-native",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image_url","image_url":{
				"url":"https://example.invalid/image.png"
			}}
		]}]
	}`)
	resp, responseBody := postMultimodalChat(t, g, body)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(responseBody), "FALLBACK") {
		t.Fatalf("response = %d %s, want native 200", resp.StatusCode, responseBody)
	}
	if basetenHits.Load() != 1 || nativeHits.Load() != 1 {
		t.Fatalf(
			"hits = Baseten %d native %d, want 1/1",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
}

func TestReactiveImage400DoesNotMoveResponsesProviderState(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			basetenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(
				w,
				`{"message":%q,"type":"Bad Request","code":400}`,
				upstreamerror.BasetenMultimodalUnsupportedMessage,
			)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			nativeHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	cfg.OpenAIURL = native.URL
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "openai"
	g, adminListener, _ := newGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	body := []byte(`{
		"model":"gpt-native",
		"previous_response_id":"resp_baseten",
		"input":[{"role":"user","content":[{
			"type":"input_image",
			"image_url":"https://example.invalid/image.png"
		}]}]
	}`)
	resp, responseBody := postMultimodalResponses(t, g, body)
	if resp.StatusCode != http.StatusBadRequest ||
		!bytes.Contains(
			responseBody,
			[]byte(upstreamerror.BasetenMultimodalUnsupportedMessage),
		) {
		t.Fatalf(
			"response = %d %s, want original Baseten 400",
			resp.StatusCode,
			responseBody,
		)
	}
	if basetenHits.Load() != 1 || nativeHits.Load() != 0 {
		t.Fatalf(
			"hits = Baseten %d native %d, want 1/0",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
}

func TestReactiveImageFallbackUsesOriginalBodyAfterResponsesNormalization(t *testing.T) {
	var basetenHits atomic.Int32
	baseten := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			basetenHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("decode normalized body: %v", err)
			} else if len(got["tools"].([]any)) != 1 ||
				len(got["input"].([]any)) != 1 {
				t.Errorf("request was not normalized before upstream: %s", body)
			}
			writeCompatibilityError(
				w,
				upstreamerror.BasetenMultimodalUnsupportedMessage,
			)
		},
	))
	defer baseten.Close()

	var nativeHits atomic.Int32
	nativeBody := make(chan []byte, 1)
	native := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			nativeHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			nativeBody <- body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"id":"resp_native","object":"response","status":"completed",` +
					`"model":"gpt-native","output":[]}`,
			))
		},
	))
	defer native.Close()

	cfg := testConfig(t, baseten.URL, baseten.URL)
	cfg.OpenAIURL = native.URL
	rc := resolvedOpenAIBaseten(t, "codex", "baseten")
	rc.DefaultModel = reasoningNeutralBasetenModel
	rc.FallbackRoute = "openai"
	rc.ResponsesCompatibility = responsesCompatibilityDefaults(t)
	rc.ResponsesCompatibility.AdditionalToolsInput = "on"
	g, adminListener, _ := newResponsesCompatibilityGateway(t, cfg, rc)
	defer adminListener.Close()
	stop := start(t, g)
	defer stop()

	original := []byte(`{
		"model":"gpt-native",
		"input":[
			{"type":"message","role":"user","content":[{
				"type":"input_image",
				"image_url":"https://example.invalid/image.png"
			}]},
			{"type":"additional_tools","tools":[
				{"type":"function","name":"shell"}
			]}
		]
	}`)
	status, _, responseBody := sendResponsesRequest(t, g, original)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, responseBody)
	}
	if basetenHits.Load() != 1 || nativeHits.Load() != 1 {
		t.Fatalf(
			"hits = Baseten %d native %d, want 1/1",
			basetenHits.Load(),
			nativeHits.Load(),
		)
	}
	select {
	case got := <-nativeBody:
		if !bytes.Equal(got, original) {
			t.Fatalf("native body changed\ngot:  %s\nwant: %s", got, original)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native request was not observed")
	}

	row := waitForRows(t, cfg.TelemetryDir, 1, 2*time.Second)[0]
	if row.Fallback.Count != 1 ||
		row.Fallback.Trigger == nil ||
		*row.Fallback.Trigger != fallbackTriggerImageUnsupported ||
		row.ResponsesCompatibility == nil {
		t.Fatalf("fallback/compatibility telemetry = %+v", row)
	}
}

func postMultimodalMessages(
	t *testing.T,
	g *Gateway,
	client string,
	body []byte,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, client, "/v1/messages"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func postMultimodalResponses(
	t *testing.T,
	g *Gateway,
	body []byte,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "codex", "/v1/responses"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func postMultimodalChat(
	t *testing.T,
	g *Gateway,
	body []byte,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		clientURL(g, "codex", "/v1/chat/completions"),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}
