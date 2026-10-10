package updates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const validFormula = `class BasetenSwitch < Formula
  url "https://github.com/basetenlabs/baseten-switch/releases/download/v0.10.0/baseten-switch_0.10.0_darwin_universal.zip"
end`

func testChecker(t *testing.T, handler http.HandlerFunc) Checker {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return Checker{Root: t.TempDir(), CurrentVersion: "v0.9.0", InstallSource: "homebrew", APIBase: server.URL, Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}
}

func feed(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/contents/") {
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(validFormula))})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.10.0", "draft": false, "prerelease": true, "published_at": "2026-01-01T00:00:00Z", "html_url": releaseBase + "v0.10.0"})
}

func TestNumericVersions(t *testing.T) {
	for _, bad := range []string{"dev", "v0.9.0-dirty", "0.9.0-beta", "v0.09.0", "v0.9", "v0.9.0.0", "v18446744073709551616.0.0"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if !IsNewer("v0.10.0", "v0.9.0") || IsNewer("v0.9.0", "v0.10.0") || IsNewer("v0.10.0", "0.10.0") {
		t.Fatal("numeric ordering failed")
	}
}

func TestCanonicalFormulaOnly(t *testing.T) {
	if tag, err := parseFormula(validFormula); err != nil || tag != "v0.10.0" {
		t.Fatalf("%q %v", tag, err)
	}
	for _, text := range []string{
		strings.ReplaceAll(validFormula, "github.com/", "github.com.example.invalid/"),
		strings.ReplaceAll(validFormula, "baseten-switch_0.10.0_darwin_universal.zip", "baseten-switch_0.9.0_darwin_universal.zip"),
		validFormula + "\n" + validFormula,
		validFormula + "\n  url \"https://example.invalid/another.zip\"",
		strings.ReplaceAll(validFormula, "v0.10.0", "v0.010.0"),
		strings.ReplaceAll(validFormula, "url \"", "url `"),
		strings.ReplaceAll(validFormula, "universal.zip\"", "universal.zip\" + ENV['TOKEN']"),
	} {
		if _, err := parseFormula(text); err == nil {
			t.Errorf("accepted invalid formula %q", text)
		}
	}
}

func TestPublishedPrereleaseAndDailyCache(t *testing.T) {
	var requests atomic.Int64
	c := testChecker(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials sent")
		}
		feed(w, r)
	})
	r, err := c.Check(context.Background(), false)
	if err != nil || r.Status != "available" || r.AvailableVersion != "v0.10.0" || r.CheckedAt == nil {
		t.Fatalf("%+v %v", r, err)
	}
	_, err = c.Check(context.Background(), false)
	if err != nil || requests.Load() != 2 {
		t.Fatalf("cached check requested metadata: %d %v", requests.Load(), err)
	}
	c.CurrentVersion = "v0.10.0"
	r, err = c.Check(context.Background(), false)
	if err != nil || r.Status != "current" || r.AvailableVersion != "v0.10.0" {
		t.Fatalf("lost canonical target for older app: %+v %v", r, err)
	}
	_, err = c.Check(context.Background(), true)
	if err != nil || requests.Load() != 4 {
		t.Fatalf("forced refresh: %d %v", requests.Load(), err)
	}
	base := c.now()
	c.Now = func() time.Time { return base.Add(24 * time.Hour) }
	_, err = c.Check(context.Background(), false)
	if err != nil || requests.Load() != 6 {
		t.Fatalf("daily check: %d %v", requests.Load(), err)
	}
}

func TestFailurePreservesTargetAndRateLimitBackoff(t *testing.T) {
	var fail atomic.Bool
	var requests atomic.Int64
	c := testChecker(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(429)
			return
		}
		feed(w, r)
	})
	first, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	r, err := c.Check(context.Background(), true)
	if err == nil || r.Error == "" || r.AvailableVersion != first.AvailableVersion || !r.CheckedAt.Equal(*first.CheckedAt) || !r.NextCheckAt.Equal(c.now().Add(2*time.Hour)) {
		t.Fatalf("failed check lost prior state: %+v %v", r, err)
	}
	count := requests.Load()
	_, err = c.Check(context.Background(), false)
	if err == nil || requests.Load() != count {
		t.Fatal("failure backoff was ignored")
	}
}

func TestAutomaticPreferenceAndUnsupportedBuild(t *testing.T) {
	var requests atomic.Int64
	c := testChecker(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); feed(w, r) })
	if err := c.SetAutomatic(false); err != nil {
		t.Fatal(err)
	}
	r, err := c.Check(context.Background(), false)
	if err != nil || r.AutomaticCheck || r.Status != "disabled" || requests.Load() != 0 {
		t.Fatalf("disabled check: %+v %v", r, err)
	}
	r, err = c.Check(context.Background(), true)
	if err != nil || r.AvailableVersion == "" || r.AutomaticCheck || requests.Load() != 2 {
		t.Fatalf("manual disabled check: %+v %v", r, err)
	}
	if err := c.SetAutomatic(true); err != nil {
		t.Fatal(err)
	}
	c.CurrentVersion = "v0.10.0-dirty"
	r, err = c.Check(context.Background(), false)
	if err != nil || r.Status != "unsupported" || requests.Load() != 2 {
		t.Fatal("development build auto-fetched")
	}
	c.Private = true
	_, err = c.Check(context.Background(), true)
	if err != nil || requests.Load() != 2 {
		t.Fatal("Preview queried public releases")
	}
}

func TestRejectDraftMismatchRedirectAndOversize(t *testing.T) {
	for _, kind := range []string{"draft", "mismatch", "unpublished", "redirect", "oversize", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			c := testChecker(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/contents/") {
					feed(w, r)
					return
				}
				switch kind {
				case "redirect":
					w.Header().Set("Location", "/unexpected")
					w.WriteHeader(302)
				case "oversize":
					fmt.Fprint(w, strings.Repeat(" ", maxResponse+1))
				case "malformed":
					fmt.Fprint(w, "{broken")
				default:
					release := map[string]any{"tag_name": "v0.10.0", "draft": false, "published_at": "2026-01-01T00:00:00Z", "html_url": releaseBase + "v0.10.0"}
					if kind == "draft" {
						release["draft"] = true
					}
					if kind == "mismatch" {
						release["tag_name"] = "v0.9.0"
					}
					if kind == "unpublished" {
						delete(release, "published_at")
					}
					_ = json.NewEncoder(w).Encode(release)
				}
			})
			r, err := c.Check(context.Background(), true)
			if err == nil || r.Status != "unknown" || r.CheckedAt != nil || r.Error == "" {
				t.Fatalf("accepted %s: %+v %v", kind, r, err)
			}
		})
	}
}

func TestConcurrentChecksShareOneFetchAndPreservePreference(t *testing.T) {
	var requests atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	c := testChecker(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
		}
		feed(w, r)
	})
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, err := c.Check(context.Background(), false)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	<-started
	if err := c.SetAutomatic(false); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if requests.Load() != 2 {
		t.Fatalf("concurrent discovery made %d requests", requests.Load())
	}
	r, err := c.Check(context.Background(), false)
	if err != nil || r.AutomaticCheck || r.AvailableVersion == "" {
		t.Fatalf("concurrent preference overwritten: %+v %v", r, err)
	}
}

func TestNoticeDedupAndDeadline(t *testing.T) {
	c := testChecker(t, feed)
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !c.TakeNotice(context.Background(), "v0.10.0") || c.TakeNotice(context.Background(), "v0.10.0") {
		t.Fatal("notice not deduplicated")
	}
	base := c.now()
	c.Now = func() time.Time { return base.Add(24 * time.Hour) }
	if !c.TakeNotice(context.Background(), "v0.10.0") {
		t.Fatal("daily notice unavailable")
	}
	lock, err := c.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Check(ctx, true); err == nil {
		t.Fatal("lock ignored deadline")
	}
	info, err := os.Stat(filepath.Join(c.Root, "cache.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("cache permissions")
	}
}

func TestCorruptCacheRecoversAndSnapshotDoesNotFetch(t *testing.T) {
	var requests atomic.Int64
	c := testChecker(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); feed(w, r) })
	if err := os.WriteFile(filepath.Join(c.Root, "cache.json"), []byte(`{"version":"v0.10.0","next_at":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAutomatic(true); err != nil {
		t.Fatal(err)
	}
	r, err := c.Snapshot()
	if err != nil || r.Status != "unknown" || requests.Load() != 0 {
		t.Fatalf("corrupt snapshot or network request: %+v %v", r, err)
	}
	r, err = c.Check(context.Background(), false)
	if err != nil || r.CheckedAt == nil || requests.Load() != 2 {
		t.Fatalf("corrupt cache blocked recovery: %+v %v", r, err)
	}
}

func TestNetworkDeadlineRecordsBackoff(t *testing.T) {
	c := testChecker(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r, err := c.Check(ctx, true)
	if err == nil || r.CheckedAt != nil || r.LastAttemptAt == nil || r.NextCheckAt == nil || !r.NextCheckAt.Equal(c.now().Add(FailureBackoff)) {
		t.Fatalf("deadline metadata: %+v %v", r, err)
	}
}
