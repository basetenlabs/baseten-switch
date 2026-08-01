package door

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startDoor builds, binds and serves a Door on an ephemeral port,
// registering cleanup. Probes default to effectively-off so the lazy
// trip logic is tested in isolation unless a test opts in.
func startDoor(t *testing.T, cfg Config) *Door {
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
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() { _ = d.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = d.Shutdown(ctx)
	})
	return d
}

// hostPort strips the scheme off an httptest server URL.
func hostPort(t *testing.T, serverURL string) string {
	t.Helper()
	return strings.TrimPrefix(serverURL, "http://")
}

// refusedAddr returns a 127.0.0.1 host:port that refuses connections.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// closeWithoutResponse simulates a router transport failure after the
// request reached the HTTP handler but before response headers were sent.
func closeWithoutResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("test response writer does not support hijacking")
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("hijack router connection: %v", err)
		return
	}
	if err := conn.Close(); err != nil {
		t.Errorf("close router connection: %v", err)
	}
}

func doorURL(d *Door, path string) string {
	return "http://" + d.Addr() + path
}

func getDoorz(t *testing.T, d *Door) Doorz {
	t.Helper()
	resp, err := http.Get(doorURL(d, "/doorz"))
	if err != nil {
		t.Fatalf("GET /doorz: %v", err)
	}
	defer resp.Body.Close()
	var z Doorz
	if err := json.NewDecoder(resp.Body).Decode(&z); err != nil {
		t.Fatalf("decode /doorz: %v", err)
	}
	return z
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

func TestStartFailsCleanlyWhenPortIsOccupied(t *testing.T) {
	owner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy test port: %v", err)
	}
	addr := owner.Addr().String()

	d, err := New(Config{
		ListenAddr:    addr,
		Shape:         ShapeAnthropic,
		RouterTarget:  "127.0.0.1:1",
		Cooldown:      time.Second,
		ProbeInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = d.Start()
	if err == nil {
		t.Fatal("Start succeeded on an occupied port")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("Start error %q does not identify occupied address %s", err, addr)
	}
	if d.listener != nil {
		t.Fatal("failed Start retained a partial listener")
	}

	if err := owner.Close(); err != nil {
		t.Fatalf("close port owner: %v", err)
	}
	replacement, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("failed Start left address unavailable: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close replacement listener: %v", err)
	}
}

// (1) Healthy path: verbatim proxying (method, path, query, headers,
// body, auth passthrough) plus per-chunk SSE streaming and
// X-Baseten-Switch-Door: router.
func TestHealthyPathProxiesVerbatimWithSSEStreaming(t *testing.T) {
	release := make(chan struct{})
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("router saw method %s", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("router saw path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("beta"); got != "true" {
			t.Errorf("router saw query beta=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer harness-token" {
			t.Errorf("router saw Authorization=%q, want harness token verbatim", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "sk-harness" {
			t.Errorf("router saw X-Api-Key=%q", got)
		}
		if got := r.Header.Get("X-Custom"); got != "custom-v" {
			t.Errorf("router saw X-Custom=%q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"m","stream":true}` {
			t.Errorf("router saw body %q", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: delta\ndata: one\n\n")
		fl.Flush()
		// Block until the test confirms the first chunk arrived at the
		// client; proves the door flushes per chunk rather than
		// buffering the whole response.
		<-release
		_, _ = io.WriteString(w, "event: delta\ndata: two\n\n")
		fl.Flush()
	}))
	defer router.Close()

	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: hostPort(t, router.URL),
		FallbackBase: "http://127.0.0.1:1", // must not be contacted
	})

	req, _ := http.NewRequest(http.MethodPost, doorURL(d, "/v1/messages?beta=true"),
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer harness-token")
	req.Header.Set("X-Api-Key", "sk-harness")
	req.Header.Set("X-Custom", "custom-v")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "router" {
		t.Fatalf("X-Baseten-Switch-Door = %q, want router", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	type lineOrErr struct {
		line string
		err  error
	}
	lines := make(chan lineOrErr, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- lineOrErr{line: sc.Text()}
		}
		lines <- lineOrErr{err: io.EOF}
	}()
	readUntil := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l := <-lines:
				if l.err != nil {
					t.Fatalf("stream ended before %q", want)
				}
				if l.line == want {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for stream line %q (first chunk not flushed?)", want)
			}
		}
	}
	readUntil("data: one")
	close(release)
	readUntil("data: two")
}

// (2) Router connect error: request transparently served by the
// fallback, client sees 200 and X-Baseten-Switch-Door: fallback.
func TestRouterConnectErrorFailsOverToFallback(t *testing.T) {
	var fallbackHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer harness-token" {
			t.Errorf("fallback saw Authorization=%q, want harness token verbatim", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: refusedAddr(t),
		FallbackBase: fallback.URL,
	})

	req, _ := http.NewRequest(http.MethodPost, doorURL(d, "/v1/messages"), strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer harness-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "fallback" {
		t.Fatalf("X-Baseten-Switch-Door = %q, want fallback", got)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback hits = %d, want 1", fallbackHits.Load())
	}
}

// (3) Every router HTTP response proves liveness. Gateway errors relay
// unchanged and never activate the door's native fallback.
func TestRouterGatewayErrorsRelayWithoutTripping(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var routerHits atomic.Int64
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				routerHits.Add(1)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "from-router")
			}))
			defer router.Close()
			var fallbackHits atomic.Int64
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackHits.Add(1)
				w.WriteHeader(200)
				_, _ = io.WriteString(w, "from-fallback")
			}))
			defer fallback.Close()

			d := startDoor(t, Config{
				Shape:        ShapeAnthropic,
				RouterTarget: hostPort(t, router.URL),
				FallbackBase: fallback.URL,
			})
			for i := 0; i < 2; i++ {
				resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader("{}"))
				if err != nil {
					t.Fatalf("request %d: %v", i, err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != status || string(body) != "from-router" {
					t.Fatalf("request %d: status=%d body=%q, want %d from-router", i, resp.StatusCode, body, status)
				}
				if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "router" {
					t.Fatalf("request %d: X-Baseten-Switch-Door = %q, want router", i, got)
				}
				if got := resp.Header.Get("Retry-After"); got != "7" {
					t.Fatalf("request %d: Retry-After = %q, want 7", i, got)
				}
			}
			if routerHits.Load() != 2 {
				t.Fatalf("router hits = %d, want 2", routerHits.Load())
			}
			if fallbackHits.Load() != 0 {
				t.Fatalf("fallback hits = %d, want 0", fallbackHits.Load())
			}
			if z := getDoorz(t, d); z.Tripped {
				t.Fatalf("door tripped after router returned %d", status)
			}
		})
	}
}

// (4) 4xx/429/500 from the router relay as-is with no trip: the next
// request still goes to the router.
func TestNonTripStatusesRelayAsIsWithoutTripping(t *testing.T) {
	for _, status := range []int{400, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var routerHits atomic.Int64
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				routerHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"router says no"}`)
			}))
			defer router.Close()
			var fallbackHits atomic.Int64
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackHits.Add(1)
				w.WriteHeader(200)
			}))
			defer fallback.Close()

			d := startDoor(t, Config{
				Shape:        ShapeAnthropic,
				RouterTarget: hostPort(t, router.URL),
				FallbackBase: fallback.URL,
			})
			for i := 0; i < 2; i++ {
				resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader("{}"))
				if err != nil {
					t.Fatalf("request %d: %v", i, err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != status {
					t.Fatalf("request %d: status = %d, want %d relayed", i, resp.StatusCode, status)
				}
				if string(body) != `{"error":"router says no"}` {
					t.Fatalf("request %d: body = %q", i, body)
				}
				if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "router" {
					t.Fatalf("request %d: X-Baseten-Switch-Door = %q, want router", i, got)
				}
			}
			if routerHits.Load() != 2 {
				t.Fatalf("router hits = %d, want 2 (must not trip)", routerHits.Load())
			}
			if fallbackHits.Load() != 0 {
				t.Fatalf("fallback hits = %d, want 0", fallbackHits.Load())
			}
			if z := getDoorz(t, d); z.Tripped {
				t.Fatalf("doorz reports tripped after %d relay", status)
			}
		})
	}
}

// (5) Cooldown: requests inside the window skip the router entirely;
// after expiry the router is probed again and recovery works.
func TestCooldownSkipsRouterThenRecovers(t *testing.T) {
	var healthy atomic.Bool
	var routerHits atomic.Int64
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routerHits.Add(1)
		if !healthy.Load() {
			_, _ = io.Copy(io.Discard, r.Body)
			closeWithoutResponse(t, w)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "from-router")
	}))
	defer router.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "from-fallback")
	}))
	defer fallback.Close()

	const cooldown = 250 * time.Millisecond
	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: hostPort(t, router.URL),
		FallbackBase: fallback.URL,
		Cooldown:     cooldown,
	})

	post := func() (*http.Response, string) {
		t.Helper()
		resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}

	// Router transport failure: first request trips (router hit 1) and
	// is served by the fallback.
	resp, body := post()
	if resp.Header.Get("X-Baseten-Switch-Door") != "fallback" || body != "from-fallback" {
		t.Fatalf("first request: door=%q body=%q, want fallback", resp.Header.Get("X-Baseten-Switch-Door"), body)
	}
	if routerHits.Load() != 1 {
		t.Fatalf("router hits = %d, want 1", routerHits.Load())
	}

	// Inside the cooldown window: the router must not be contacted.
	resp, body = post()
	if resp.Header.Get("X-Baseten-Switch-Door") != "fallback" || body != "from-fallback" {
		t.Fatalf("cooldown request: door=%q body=%q, want fallback", resp.Header.Get("X-Baseten-Switch-Door"), body)
	}
	if routerHits.Load() != 1 {
		t.Fatalf("router hits = %d after cooldown-window request, want 1 (router must be skipped)", routerHits.Load())
	}

	// After expiry with a healthy router: probed again, recovers.
	healthy.Store(true)
	time.Sleep(cooldown + 50*time.Millisecond)
	resp, body = post()
	if resp.Header.Get("X-Baseten-Switch-Door") != "router" || body != "from-router" {
		t.Fatalf("post-cooldown request: door=%q body=%q, want router", resp.Header.Get("X-Baseten-Switch-Door"), body)
	}
	if routerHits.Load() != 2 {
		t.Fatalf("router hits = %d, want 2", routerHits.Load())
	}
	if z := getDoorz(t, d); z.Tripped {
		t.Fatal("doorz still tripped after recovery")
	}
}

// (6) Health probe trips proactively on /healthz failure and clears the
// trip early on success (before the cooldown would expire).
func TestHealthProbeTripsAndRecoversProactively(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if healthy.Load() {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(503)
			}
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "from-router")
	}))
	defer router.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "from-fallback")
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		Shape:         ShapeAnthropic,
		RouterTarget:  hostPort(t, router.URL),
		FallbackBase:  fallback.URL,
		Cooldown:      time.Hour, // proves probe success clears the trip early
		ProbeInterval: 20 * time.Millisecond,
	})

	waitFor(t, 2*time.Second, func() bool { return !getDoorz(t, d).Tripped }, "initial untripped state")

	healthy.Store(false)
	waitFor(t, 2*time.Second, func() bool { return getDoorz(t, d).Tripped }, "probe to trip proactively")

	// While tripped, requests serve from the fallback without touching
	// the router's request path.
	resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Baseten-Switch-Door") != "fallback" || string(body) != "from-fallback" {
		t.Fatalf("tripped request: door=%q body=%q, want fallback", resp.Header.Get("X-Baseten-Switch-Door"), body)
	}

	healthy.Store(true)
	waitFor(t, 2*time.Second, func() bool { return !getDoorz(t, d).Tripped }, "probe to clear the trip early")

	resp, err = http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Baseten-Switch-Door") != "router" || string(body) != "from-router" {
		t.Fatalf("recovered request: door=%q body=%q, want router", resp.Header.Get("X-Baseten-Switch-Door"), body)
	}
}

// (7) Request body replay: on a trip the same POST body reaches the
// fallback intact.
func TestRequestBodyReplayOnTrip(t *testing.T) {
	const payload = `{"model":"claude","messages":[{"role":"user","content":"replay me exactly"}]}`
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the body (as a real router would) before failing.
		_, _ = io.Copy(io.Discard, r.Body)
		closeWithoutResponse(t, w)
	}))
	defer router.Close()
	gotBody := make(chan []byte, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.WriteHeader(200)
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: hostPort(t, router.URL),
		FallbackBase: fallback.URL,
	})
	resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Baseten-Switch-Door") != "fallback" {
		t.Fatalf("status=%d door=%q, want 200 fallback", resp.StatusCode, resp.Header.Get("X-Baseten-Switch-Door"))
	}
	select {
	case b := <-gotBody:
		if !bytes.Equal(b, []byte(payload)) {
			t.Fatalf("fallback body = %q, want %q", b, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fallback never received the replayed body")
	}
}

// Bodies over the replay cap go router-only: a router transport failure
// cannot replay that request, but the trip still protects subsequent
// replayable requests.
func TestOversizeBodyIsRouterOnly(t *testing.T) {
	bigBody := strings.Repeat("x", 64)
	var routerBody atomic.Value
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		routerBody.Store(string(b))
		closeWithoutResponse(t, w)
	}))
	defer router.Close()
	var fallbackHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(200)
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: hostPort(t, router.URL),
		FallbackBase: fallback.URL,
		MaxReplay:    16,
	})
	resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader(bigBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want door 502 for unreplayable transport failure", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "router" {
		t.Fatalf("X-Baseten-Switch-Door = %q, want router", got)
	}
	if got, _ := routerBody.Load().(string); got != bigBody {
		t.Fatalf("router received %d body bytes, want the full %d", len(got), len(bigBody))
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("fallback hits = %d, want 0 for an unreplayable body", fallbackHits.Load())
	}
	// The trip still landed: a small follow-up request fails over.
	resp, err = http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("follow-up request: %v", err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Baseten-Switch-Door") != "fallback" {
		t.Fatalf("follow-up X-Baseten-Switch-Door = %q, want fallback (trip must persist)", resp.Header.Get("X-Baseten-Switch-Door"))
	}
}

// Unknown path while tripped: 502 with a JSON error naming the door.
func TestUnknownPathWhileTrippedReturns502JSON(t *testing.T) {
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fallback must not be contacted for a non-allowlisted path (got %s)", r.URL.Path)
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: refusedAddr(t),
		FallbackBase: fallback.URL,
	})
	// Trip via an allowlist miss is fine too, but path checking happens
	// on the fallback path either way; hit an unknown path directly.
	resp, err := http.Post(doorURL(d, "/v1/admin/config"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Door struct {
			Port  int    `json:"port"`
			Shape string `json:"shape"`
		} `json:"door"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("502 body is not JSON: %v (%q)", err, body)
	}
	if parsed.Error.Type != "baseten_switch_door_error" || !strings.Contains(parsed.Error.Message, "/v1/admin/config") {
		t.Fatalf("unexpected error payload: %q", body)
	}
	if parsed.Door.Shape != "anthropic" || parsed.Door.Port == 0 {
		t.Fatalf("error must name the door: %q", body)
	}
}

// (8) /doorz reports state and transitions.
func TestDoorzReportsStateTransitions(t *testing.T) {
	routerAddr := refusedAddr(t)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer fallback.Close()

	const cooldown = 30 * time.Second
	d := startDoor(t, Config{
		Shape:        ShapeOpenAI,
		RouterTarget: routerAddr,
		FallbackBase: fallback.URL,
		Cooldown:     cooldown,
	})

	z := getDoorz(t, d)
	wantPort := 0
	if _, p, err := net.SplitHostPort(d.Addr()); err == nil {
		fmt.Sscanf(p, "%d", &wantPort)
	}
	if z.Port != wantPort || z.Shape != "openai" || z.Router != routerAddr {
		t.Fatalf("doorz identity = %+v, want port=%d shape=openai router=%s", z, wantPort, routerAddr)
	}
	if z.Tripped || z.CooldownRemainingMS != 0 || z.LastTransitionUnix != 0 {
		t.Fatalf("fresh doorz should be untripped with no transition: %+v", z)
	}

	before := time.Now().Unix()
	resp, err := http.Post(doorURL(d, "/v1/chat/completions"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	z = getDoorz(t, d)
	if !z.Tripped {
		t.Fatalf("doorz not tripped after router transport failure: %+v", z)
	}
	if z.CooldownRemainingMS <= 0 || z.CooldownRemainingMS > cooldown.Milliseconds() {
		t.Fatalf("cooldown_remaining_ms = %d, want in (0, %d]", z.CooldownRemainingMS, cooldown.Milliseconds())
	}
	if z.LastTransitionUnix < before || z.LastTransitionUnix > time.Now().Unix()+1 {
		t.Fatalf("last_transition_unix = %d, want ~now", z.LastTransitionUnix)
	}
}

// /doorz carries the door's configured fallback base so dashboards can
// show the real failover target instead of assuming the shape default.
func TestDoorzIncludesFallbackBase(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer router.Close()

	d := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: hostPort(t, router.URL),
		FallbackBase: "https://fallback.example.com",
	})

	resp, err := http.Get(doorURL(d, "/doorz"))
	if err != nil {
		t.Fatalf("GET /doorz: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode /doorz: %v", err)
	}
	if got, _ := raw["fallback_base"].(string); got != "https://fallback.example.com" {
		t.Fatalf("doorz fallback_base = %q, want https://fallback.example.com", got)
	}

	// Unset in config: /doorz reports the shape's hardcoded default.
	d2 := startDoor(t, Config{
		Shape:        ShapeOpenAI,
		RouterTarget: hostPort(t, router.URL),
	})
	if z := getDoorz(t, d2); z.FallbackBase != DefaultOpenAIBase {
		t.Fatalf("doorz fallback_base = %q, want %q", z.FallbackBase, DefaultOpenAIBase)
	}
}

// Path-aware fallback on a shared-shape port: while tripped, each
// request goes to the native base owning its path prefix, /v1/models
// disambiguates on the anthropic-version header, and unowned paths
// still 502.
func TestTrippedSharedPortPicksFallbackByPath(t *testing.T) {
	native := func(tag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, tag)
		}))
	}
	aFall := native("anthropic-native")
	defer aFall.Close()
	oFall := native("openai-native")
	defer oFall.Close()

	d := startDoor(t, Config{
		RouterTarget: refusedAddr(t),
		Fallbacks: []FallbackRule{
			{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: aFall.URL},
			{Prefix: "/v1/chat/completions", Shape: ShapeOpenAI, Base: oFall.URL},
			{Prefix: "/v1/responses", Shape: ShapeOpenAI, Base: oFall.URL},
		},
	})

	cases := []struct {
		name     string
		method   string
		path     string
		header   map[string]string
		wantBody string
		want502  bool
	}{
		{name: "anthropic messages", method: "POST", path: "/v1/messages", wantBody: "anthropic-native"},
		{name: "anthropic count_tokens by prefix", method: "POST", path: "/v1/messages/count_tokens", wantBody: "anthropic-native"},
		{name: "openai chat", method: "POST", path: "/v1/chat/completions", wantBody: "openai-native"},
		{name: "openai responses", method: "POST", path: "/v1/responses", wantBody: "openai-native"},
		{name: "models with anthropic-version header", method: "GET", path: "/v1/models",
			header: map[string]string{"anthropic-version": "2023-06-01"}, wantBody: "anthropic-native"},
		{name: "models without header", method: "GET", path: "/v1/models", wantBody: "openai-native"},
		{name: "unowned path 502", method: "POST", path: "/v1/admin/config", want502: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, doorURL(d, tc.path), strings.NewReader("{}"))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if tc.want502 {
				if resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (body: %s)", resp.StatusCode, body)
				}
				return
			}
			if resp.StatusCode != 200 || string(body) != tc.wantBody {
				t.Fatalf("status=%d body=%q, want 200 %q", resp.StatusCode, body, tc.wantBody)
			}
			if got := resp.Header.Get("X-Baseten-Switch-Door"); got != "fallback" {
				t.Fatalf("X-Baseten-Switch-Door = %q, want fallback", got)
			}
		})
	}
}

// Single-shape rules keep today's behavior: anthropic paths and
// /v1/models go to the one base, everything else (including the other
// shape's paths) still 502s.
func TestTrippedSingleShapeRulesMatchCurrentBehavior(t *testing.T) {
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "anthropic-native")
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		RouterTarget: refusedAddr(t),
		FallbackBase: fallback.URL,
		Fallbacks: []FallbackRule{
			{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: fallback.URL},
		},
	})
	if got := d.cfg.Shape; got != ShapeAnthropic {
		t.Fatalf("derived shape = %q, want %q", got, ShapeAnthropic)
	}

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/models"} {
		resp, err := http.Post(doorURL(d, path), "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "anthropic-native" {
			t.Fatalf("%s: status=%d body=%q, want 200 anthropic-native", path, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/admin/config"} {
		resp, err := http.Post(doorURL(d, path), "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status=%d body=%q, want 502", path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "no native fallback for shape anthropic") {
			t.Fatalf("%s: body %q must name the shape", path, body)
		}
	}
}

// /doorz lists the resolved fallback rules as {prefix, base} pairs and
// keeps fallback_base; rule-less doors omit the key entirely.
func TestDoorzIncludesFallbacks(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer router.Close()

	d := startDoor(t, Config{
		RouterTarget: hostPort(t, router.URL),
		Fallbacks: []FallbackRule{
			{Prefix: "/v1/messages", Shape: ShapeAnthropic, Base: "https://a.example.com"},
			{Prefix: "/v1/chat/completions", Shape: ShapeOpenAI, Base: "https://o.example.com"},
		},
	})
	resp, err := http.Get(doorURL(d, "/doorz"))
	if err != nil {
		t.Fatalf("GET /doorz: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode /doorz: %v", err)
	}
	if got, _ := raw["fallback_base"].(string); got != "https://a.example.com" {
		t.Fatalf("fallback_base = %q, want the anthropic rule base", got)
	}
	if got, _ := raw["shape"].(string); got != "anthropic+openai" {
		t.Fatalf("shape = %q, want anthropic+openai", got)
	}
	rules, _ := raw["fallbacks"].([]any)
	want := []map[string]string{
		{"prefix": "/v1/messages", "base": "https://a.example.com"},
		{"prefix": "/v1/chat/completions", "base": "https://o.example.com"},
	}
	if len(rules) != len(want) {
		t.Fatalf("fallbacks = %v, want %d entries", rules, len(want))
	}
	for i, w := range want {
		entry, _ := rules[i].(map[string]any)
		if len(entry) != 2 || entry["prefix"] != w["prefix"] || entry["base"] != w["base"] {
			t.Fatalf("fallbacks[%d] = %v, want %v (prefix and base only)", i, entry, w)
		}
	}

	// No rules: the optional key is omitted.
	d2 := startDoor(t, Config{
		Shape:        ShapeAnthropic,
		RouterTarget: hostPort(t, router.URL),
	})
	resp2, err := http.Get(doorURL(d2, "/doorz"))
	if err != nil {
		t.Fatalf("GET /doorz: %v", err)
	}
	defer resp2.Body.Close()
	var raw2 map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&raw2); err != nil {
		t.Fatalf("decode /doorz: %v", err)
	}
	if _, present := raw2["fallbacks"]; present {
		t.Fatalf("rule-less doorz must omit fallbacks: %v", raw2)
	}
}

// A router that accepts connections but never answers (wedged process)
// must not strand in-flight requests: the health probe trips the door
// and the trip rescues pre-header forwards into the fallback replay.
// Added 2026-07-09; before this, such requests hung until the harness
// gave up (the door has no fixed header deadline on purpose: router
// headers legitimately arrive late when a model is slow to first
// token, which is the router ttft_timeout's job).
func TestTripRescuesInflightRequestBehindWedgedRouter(t *testing.T) {
	wedged := make(chan struct{})
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-wedged // hang everything, /healthz included
	}))
	defer router.Close()
	defer close(wedged)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "rescued-by-fallback")
	}))
	defer fallback.Close()

	d := startDoor(t, Config{
		Shape:         ShapeAnthropic,
		RouterTarget:  hostPort(t, router.URL),
		FallbackBase:  fallback.URL,
		Cooldown:      time.Hour,
		ProbeInterval: 20 * time.Millisecond, // probe timeout tracks the interval
	})

	// Fire the request while the router wedges; the probe trips within
	// ~40ms and must rescue this pre-header forward.
	type result struct {
		body string
		via  string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Post(doorURL(d, "/v1/messages"), "application/json", strings.NewReader(`{"probe":true}`))
		if err != nil {
			done <- result{err: err}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- result{body: string(body), via: resp.Header.Get("X-Baseten-Switch-Door")}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("rescued request errored: %v", res.err)
		}
		if res.via != "fallback" || res.body != "rescued-by-fallback" {
			t.Fatalf("rescued request: door=%q body=%q, want fallback replay", res.via, res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request was not rescued; still hanging behind the wedged router")
	}

	if !getDoorz(t, d).Tripped {
		t.Fatal("door not tripped after wedged-router rescue")
	}
}
