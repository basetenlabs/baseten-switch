package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func startGatewayDoor(t *testing.T, cfg door.Config) *door.Door {
	t.Helper()
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = time.Second
	}
	if cfg.ProbeInterval == 0 {
		cfg.ProbeInterval = time.Hour
	}
	if cfg.Logf == nil {
		cfg.Logf = t.Logf
	}
	d, err := door.New(cfg)
	if err != nil {
		t.Fatalf("door.New: %v", err)
	}
	if err := d.Start(); err != nil {
		t.Fatalf("door.Start: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = d.Serve()
		close(done)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := d.Shutdown(ctx); err != nil {
			t.Errorf("door.Shutdown: %v", err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("door did not stop")
		}
	})
	return d
}

func TestDoorRelaysRouterHTTPErrorWithoutNativeFallback(t *testing.T) {
	const responseBody = `{"type":"error","error":{"type":"overloaded_error","message":"synthetic overload"}}`
	var routerUpstreamHits atomic.Int64
	routerUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routerUpstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer routerUpstream.Close()

	var nativeHits atomic.Int64
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"unexpected native response"}]}`)
	}))
	defer native.Close()

	cfg := testConfig(t, routerUpstream.URL, native.URL)
	rc := resolvedAnthropicBaseten(t)
	rc.FallbackRoute = ""
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	routerAddr := g.ClientAddr(rc.Name).(*net.TCPAddr).String()
	d := startGatewayDoor(t, door.Config{
		Shape:        door.ShapeAnthropic,
		RouterTarget: routerAddr,
		FallbackBase: native.URL,
	})
	doorEndpoint := "http://" + d.Addr() + "/v1/messages"
	requestBody := []byte(`{"model":"claude-example","messages":[{"role":"user","content":"synthetic request"}]}`)

	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, doorEndpoint, bytes.NewReader(requestBody))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("request %d body: %v", i, err)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("request %d status = %d, want 503", i, resp.StatusCode)
		}
		if string(body) != responseBody {
			t.Fatalf("request %d body = %q, want %q", i, body, responseBody)
		}
		if got := resp.Header.Get("Retry-After"); got != "9" {
			t.Fatalf("request %d Retry-After = %q, want 9", i, got)
		}
		if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "router" {
			t.Fatalf("request %d X-Baseten-Switch-Door = %q, want router", i, got)
		}
	}

	if got := routerUpstreamHits.Load(); got != 2 {
		t.Fatalf("router upstream hits = %d, want 2", got)
	}
	if got := nativeHits.Load(); got != 0 {
		t.Fatalf("native fallback hits = %d, want 0", got)
	}

	doorResp, err := http.Get("http://" + d.Addr() + "/doorz")
	if err != nil {
		t.Fatalf("GET /doorz: %v", err)
	}
	defer doorResp.Body.Close()
	var doorStatus door.Doorz
	if err := json.NewDecoder(doorResp.Body).Decode(&doorStatus); err != nil {
		t.Fatalf("decode /doorz: %v", err)
	}
	if doorStatus.Tripped {
		t.Fatalf("door tripped after router HTTP responses: %+v", doorStatus)
	}

	rows := waitForRows(t, cfg.TelemetryDir, 2, 2*time.Second)
	for i, row := range rows {
		if row.StatusCode() != http.StatusServiceUnavailable ||
			row.TerminationReason != telemetry.TerminationUpstreamHTTPError {
			t.Fatalf("telemetry row %d status/termination = %d/%q, want 503/%q",
				i, row.StatusCode(), row.TerminationReason, telemetry.TerminationUpstreamHTTPError)
		}
		if row.EffectiveProvider != "baseten" {
			t.Fatalf("telemetry row %d effective_provider = %q, want baseten", i, row.EffectiveProvider)
		}
		if row.Fallback.Attempted || row.Fallback.Count != 0 || row.Fallback.Trigger != nil {
			t.Fatalf("telemetry row %d fallback = %+v, want no attempt", i, row.Fallback)
		}
	}
}
