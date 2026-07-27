package gateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

// TestReloadConfigConcurrentRequests exercises the two runtime snapshots that
// a SIGHUP swaps: process-wide Config and immutable per-client routing. Run
// this under -race; status and request handlers must never read a partially
// replaced Config while reloadConfig is publishing the new resolver.
func TestReloadConfigConcurrentRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	rc := resolvedAnthropicBaseten(t)
	rc.BindAddr = "127.0.0.1:0"
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{rc})
	g, adminL, _ := newGateway(t, cfg, rc)
	defer adminL.Close()
	stop := start(t, g)
	defer stop()

	const reloads = 24
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < reloads; j++ {
				for _, path := range []string{"/v1/admin/status", "/healthz"} {
					resp, err := http.Get(adminURL(g, path))
					if err != nil {
						errCh <- err
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						errCh <- fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < reloads; i++ {
		next := rc
		if i%2 == 1 {
			next.Route = "anthropic"
		}
		writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{next})
		g.reloadConfig()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestReloadRetiresListenerBeforePublishingConfig verifies that a topology
// reload closes the old accept loop before it advertises the new generation.
// A request already running on the old immutable client finishes, while an
// idle keep-alive or a new connection cannot serve stale policy afterward.
func TestReloadRetiresListenerBeforePublishingConfig(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "gateway.yaml")
	old := resolvedAnthropicBaseten(t)
	old.BindAddr = "127.0.0.1:" + itoa(freeTCPPort(t))
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{old})
	adminL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, pricing.New(), adminL, []resolvedClientConfig{old})
	if err != nil {
		adminL.Close()
		t.Fatal(err)
	}
	defer adminL.Close()
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- g.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("Serve returned during shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return during shutdown")
		}
	}()
	pollHealthz(t, g)

	conn, err := net.Dial("tcp", old.BindAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writeKeepAliveRequest(t, conn)
	first, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("initial health status = %d, want 200", first.StatusCode)
	}
	inflight := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, clientURL(g, old.Name, "/v1/messages"),
			bytes.NewBufferString(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			inflight <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			inflight <- err
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			inflight <- fmt.Errorf("in-flight status = %d", resp.StatusCode)
			return
		}
		inflight <- nil
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old request did not reach upstream")
	}

	next := resolvedAnthropicBaseten(t)
	next.Name = "replacement"
	next.BindAddr = "127.0.0.1:" + itoa(freeTCPPort(t))
	next.Route = "anthropic"
	writeGatewayYAML(t, cfg.ConfigPath, []resolvedClientConfig{next})
	g.reloadConfig()
	// stopAccepting closes the listener that Serve originally launched. Give
	// that accept loop time to return, then assert its intentional closure did
	// not terminate the main Serve loop.
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned after intentional listener closure: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-inflight:
		if err != nil {
			t.Fatalf("in-flight request did not complete: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request was not allowed to complete")
	}

	if got := g.ClientAddr(old.Name); got != nil {
		t.Fatalf("retired client remains published at %s", got)
	}
	if got := g.ClientAddr(next.Name); got == nil {
		t.Fatal("replacement client was not published")
	}
	writeKeepAliveRequest(t, conn)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := http.ReadResponse(reader, nil)
	if err == nil {
		_, _ = io.Copy(io.Discard, second.Body)
		second.Body.Close()
		if second.StatusCode == http.StatusOK {
			t.Fatalf("retired keep-alive served stale policy with status %d", second.StatusCode)
		}
	}

	probe, err := net.DialTimeout("tcp", old.BindAddr, 500*time.Millisecond)
	if err == nil {
		defer probe.Close()
		writeKeepAliveRequest(t, probe)
		_ = probe.SetReadDeadline(time.Now().Add(2 * time.Second))
		probeResp, probeErr := http.ReadResponse(bufio.NewReader(probe), nil)
		if probeErr == nil {
			_, _ = io.Copy(io.Discard, probeResp.Body)
			probeResp.Body.Close()
			if probeResp.StatusCode == http.StatusOK {
				t.Fatal("new connection to retired listener served stale policy")
			}
		}
	}
}

func writeKeepAliveRequest(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := fmt.Fprint(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\nConnection: keep-alive\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
}
