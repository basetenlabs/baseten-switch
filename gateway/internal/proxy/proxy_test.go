package proxy

import (
	"errors"
	"net/http"
	"testing"
)

func TestBuildUpstreamHeadersStripsHopByHopAndAuth(t *testing.T) {
	in := http.Header{}
	in.Set("Connection", "keep-alive")
	in.Set("Transfer-Encoding", "chunked")
	in.Set("Content-Length", "123")
	in.Set("Host", "localhost:8787")
	in.Set("Authorization", "Bearer client-token")
	in.Set("X-Api-Key", "client-anthropic-key")
	in.Set("Accept-Encoding", "gzip, deflate")
	in.Set("Anthropic-Version", "2023-06-01")
	in.Set("Content-Type", "application/json")
	in.Set("X-Custom", "keep-me")

	out, err := BuildUpstreamHeaders(in, UpstreamModeAPIKey, "bas-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, banned := range []string{"Connection", "Transfer-Encoding", "Content-Length", "Host", "X-Api-Key", "Accept-Encoding"} {
		if out.Get(banned) != "" {
			t.Fatalf("hop-by-hop %q leaked: %q", banned, out.Get(banned))
		}
	}
	if out.Get("Authorization") != "Api-Key bas-secret" {
		t.Fatalf("baseten auth not set: %q", out.Get("Authorization"))
	}
	if out.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("forward header dropped: %q", out.Get("Anthropic-Version"))
	}
	if out.Get("X-Custom") != "keep-me" {
		t.Fatalf("custom header dropped: %q", out.Get("X-Custom"))
	}
}

func TestRewriteModelWithTarget(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"messages":[]}`)
	res := RewriteModelInBody(body, "zai-org/GLM-5.2")
	if res.RequestedModel != "claude-opus-4-8" {
		t.Fatalf("requested = %q", res.RequestedModel)
	}
	if res.UpstreamModel != "zai-org/GLM-5.2" {
		t.Fatalf("upstream = %q", res.UpstreamModel)
	}
	if !res.IsStream {
		t.Fatal("stream not detected")
	}
	if !contains(string(res.NewBody), `"model":"zai-org/GLM-5.2"`) {
		t.Fatalf("rewritten body missing upstream model: %s", res.NewBody)
	}
}

func TestRewriteModelPassthrough(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
	res := RewriteModelInBody(body, "")
	if res.UpstreamModel != "claude-opus-4-8" {
		t.Fatalf("passthrough upstream should equal requested, got %q", res.UpstreamModel)
	}
	if string(res.NewBody) != string(body) {
		t.Fatalf("passthrough body should be untouched:\nwant %s\ngot  %s", body, res.NewBody)
	}
}

func TestRewriteModelSameModelNoRewrite(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
	res := RewriteModelInBody(body, "claude-opus-4-8")
	if res.UpstreamModel != "claude-opus-4-8" {
		t.Fatalf("upstream should equal requested when target matches, got %q", res.UpstreamModel)
	}
	if string(res.NewBody) != string(body) {
		t.Fatalf("body should be untouched when target matches:\nwant %s\ngot  %s", body, res.NewBody)
	}
}

func TestRewriteModelInvalidJSON(t *testing.T) {
	body := []byte(`{not json`)
	res := RewriteModelInBody(body, "zai-org/GLM-5.2")
	if res.Parsed {
		t.Fatal("should report not parsed for invalid json")
	}
	if string(res.NewBody) != string(body) {
		t.Fatal("invalid body should pass through unchanged")
	}
	if res.RequestedModel != "" || res.UpstreamModel != "" {
		t.Fatalf("models should be empty on parse error")
	}
}

func TestRewriteModelNoModelField(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	res := RewriteModelInBody(body, "zai-org/GLM-5.2")
	if res.RequestedModel != "" {
		t.Fatalf("requested should be empty, got %q", res.RequestedModel)
	}
	if res.UpstreamModel != "zai-org/GLM-5.2" {
		t.Fatalf("upstream should fall back to target, got %q", res.UpstreamModel)
	}
}

func TestBuildUpstreamHeadersPassthrough(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer xyz")
	in.Set("X-Api-Key", "client-anthropic-key")
	in.Set("Content-Type", "application/json")
	out, err := BuildUpstreamHeaders(in, UpstreamModePassthrough, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Get("Authorization") != "Bearer xyz" {
		t.Fatalf("passthrough should preserve inbound Authorization: %q", out.Get("Authorization"))
	}
	if out.Get("X-Api-Key") != "client-anthropic-key" {
		t.Fatalf("passthrough should preserve inbound X-Api-Key: %q", out.Get("X-Api-Key"))
	}
	if out.Get("Content-Type") != "application/json" {
		t.Fatalf("passthrough dropped unrelated header: %q", out.Get("Content-Type"))
	}
}

func TestBuildUpstreamHeadersOAuth(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer client")
	in.Set("X-Api-Key", "client-anthropic-key")
	in.Set("X-Custom", "keep-me")
	out, err := BuildUpstreamHeaders(in, UpstreamModeOAuth, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Get("Authorization") != "" {
		t.Fatalf("oauth mode must strip inbound Authorization: %q", out.Get("Authorization"))
	}
	if out.Get("X-Api-Key") != "" {
		t.Fatalf("oauth mode must strip inbound X-Api-Key: %q", out.Get("X-Api-Key"))
	}
	if out.Get("X-Custom") != "keep-me" {
		t.Fatalf("oauth mode dropped unrelated header: %q", out.Get("X-Custom"))
	}
}

func TestBuildUpstreamHeadersAPIKey(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer client")
	in.Set("X-Api-Key", "client-anthropic-key")
	out, err := BuildUpstreamHeaders(in, UpstreamModeAPIKey, "mykey")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Get("Authorization") != "Api-Key mykey" {
		t.Fatalf("apikey mode should set Authorization to Api-Key mykey: %q", out.Get("Authorization"))
	}
	if out.Get("X-Api-Key") != "" {
		t.Fatalf("apikey mode should not set X-Api-Key: %q", out.Get("X-Api-Key"))
	}
}

// TestBuildUpstreamHeadersAPIKeyEmpty verifies the empty-key hard guard:
// APIKey mode with no key must be an explicit error, never a silent strip
// that sends an unauthenticated upstream request.
func TestBuildUpstreamHeadersAPIKeyEmpty(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer client")
	out, err := BuildUpstreamHeaders(in, UpstreamModeAPIKey, "")
	if err == nil {
		t.Fatal("apikey mode with empty key must return an error")
	}
	if !errors.Is(err, ErrEmptyAPIKey) {
		t.Fatalf("error = %v, want ErrEmptyAPIKey", err)
	}
	if out != nil {
		t.Fatalf("headers should be nil on error, got %v", out)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
