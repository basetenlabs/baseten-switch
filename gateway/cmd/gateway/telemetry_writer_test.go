package gateway

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func TestReloadConfigDisablesAndLazilyReenablesTelemetry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	writeTelemetryGatewayConfig(t, configPath, dir, true, 90)
	cfg := testConfig(t, "", "")
	cfg.ConfigPath = configPath
	resolved, err := loadResolvedClients(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, pricing.New(), adminListener, resolved)
	if err != nil {
		_ = adminListener.Close()
		t.Fatal(err)
	}
	defer func() {
		if err := g.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		_ = adminListener.Close()
	}()

	g.writeTelemetryV1(gatewayTelemetryEvent(1))
	if got := telemetryLineCount(t, dir); got != 1 {
		t.Fatalf("initial telemetry line count = %d, want 1", got)
	}

	writeTelemetryGatewayConfig(t, configPath, dir, false, 90)
	g.reloadConfig()
	if g.telemetryWriter != nil {
		t.Fatal("disabled reload left telemetry writer open")
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(2))
	if got := telemetryLineCount(t, dir); got != 1 {
		t.Fatalf("disabled telemetry line count = %d, want 1", got)
	}

	writeTelemetryGatewayConfig(t, configPath, dir, true, 30)
	g.reloadConfig()
	if g.telemetryWriter != nil {
		t.Fatal("enabled reload opened telemetry writer before an event")
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(2))
	if g.telemetryWriter == nil || g.telemetryRetentionDays != 30 {
		t.Fatalf("re-enabled writer = %v, retention = %d", g.telemetryWriter, g.telemetryRetentionDays)
	}
	if got := telemetryLineCount(t, dir); got != 2 {
		t.Fatalf("re-enabled telemetry line count = %d, want 2", got)
	}
}

func TestTelemetryWriterReloadDisableAndReenableIsLazy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	disabled := false
	cfg := Config{
		TelemetryDir:           dir,
		TelemetryEnabled:       &disabled,
		TelemetryRetentionDays: 90,
	}
	g := &Gateway{cfg: cfg}
	g.writeTelemetryV1(gatewayTelemetryEvent(1))
	if segments, err := telemetry.DiscoverSegments(dir); err != nil || len(segments) != 0 {
		t.Fatalf("disabled telemetry segments = %v, error = %v", segments, err)
	}

	enabled := true
	cfg.TelemetryEnabled = &enabled
	g.setRuntimeConfig(cfg)
	g.reconcileTelemetryWriter(cfg)
	if g.telemetryWriter != nil {
		t.Fatal("reload enable opened writer before an event")
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(1))
	if g.telemetryWriter == nil {
		t.Fatal("enabled telemetry did not lazily open writer")
	}
	if got := telemetrySegmentCount(t, dir); got != 1 {
		t.Fatalf("segment count = %d, want 1", got)
	}

	cfg.TelemetryEnabled = &disabled
	g.setRuntimeConfig(cfg)
	g.reconcileTelemetryWriter(cfg)
	if g.telemetryWriter != nil {
		t.Fatal("reload disable left writer open")
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(2))
	if got := telemetrySegmentCount(t, dir); got != 1 {
		t.Fatalf("disabled write changed segment count to %d", got)
	}

	cfg.TelemetryEnabled = &enabled
	g.setRuntimeConfig(cfg)
	g.reconcileTelemetryWriter(cfg)
	if g.telemetryWriter != nil {
		t.Fatal("reload re-enable opened writer before an event")
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(2))
	if g.telemetryWriter == nil {
		t.Fatal("re-enabled telemetry did not resume")
	}
	if err := g.closeTelemetryWriter(); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryWriterReloadChangesDirectoryAndRetention(t *testing.T) {
	firstDir := filepath.Join(t.TempDir(), "first")
	secondDir := filepath.Join(t.TempDir(), "second")
	enabled := true
	cfg := Config{
		TelemetryDir:           firstDir,
		TelemetryEnabled:       &enabled,
		TelemetryRetentionDays: 90,
	}
	g := &Gateway{cfg: cfg}
	g.writeTelemetryV1(gatewayTelemetryEvent(1))
	if g.telemetryWriter == nil {
		t.Fatal("initial writer did not open")
	}

	cfg.TelemetryDir = secondDir
	cfg.TelemetryRetentionDays = 30
	g.setRuntimeConfig(cfg)
	g.reconcileTelemetryWriter(cfg)
	if g.telemetryWriter != nil {
		t.Fatal("settings reload did not close old writer")
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(2))
	if g.telemetryWriter == nil ||
		g.telemetryWriterDir != secondDir ||
		g.telemetryRetentionDays != 30 {
		t.Fatalf(
			"writer settings = %q, %d",
			g.telemetryWriterDir,
			g.telemetryRetentionDays,
		)
	}
	if got := telemetrySegmentCount(t, firstDir); got != 1 {
		t.Fatalf("first store segment count = %d, want 1", got)
	}
	if got := telemetrySegmentCount(t, secondDir); got != 1 {
		t.Fatalf("second store segment count = %d, want 1", got)
	}
	if err := g.closeTelemetryWriter(); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayShutdownClosesAndStopsTelemetryWriter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	enabled := true
	g := &Gateway{
		cfg: Config{
			TelemetryDir:           dir,
			TelemetryEnabled:       &enabled,
			TelemetryRetentionDays: 90,
		},
		adminServer:  &http.Server{},
		groups:       map[string]*listenerGroup{},
		authTickStop: make(chan struct{}),
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(1))
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.telemetryWriter != nil || !g.telemetryStopped {
		t.Fatal("Shutdown did not stop telemetry writer")
	}

	other, err := telemetry.NewWriter(telemetry.WriterOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Shutdown did not release telemetry writer ownership: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	g.writeTelemetryV1(gatewayTelemetryEvent(2))
	if got := telemetrySegmentCount(t, dir); got != 1 {
		t.Fatalf("post-shutdown write changed segment count to %d", got)
	}
}

func TestTelemetryAdminHealthPersistsWhenCollectionDisabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	enabled := true
	cfg := Config{
		TelemetryDir:           dir,
		TelemetryEnabled:       &enabled,
		TelemetryRetentionDays: 90,
	}
	g := &Gateway{cfg: cfg}
	g.writeTelemetryV1(gatewayTelemetryEvent(1))

	health := g.telemetryAdminHealth(cfg)
	if health["collection_enabled"] != true ||
		health["active_segment"] == "" ||
		health["last_write_at"] == nil ||
		health["retention_days"] != 90 ||
		health["dropped_events"] != uint64(0) {
		t.Fatalf("enabled health = %+v", health)
	}

	enabled = false
	cfg.TelemetryEnabled = &enabled
	g.setRuntimeConfig(cfg)
	g.reconcileTelemetryWriter(cfg)
	health = g.telemetryAdminHealth(cfg)
	if health["collection_enabled"] != false ||
		health["active_segment"] == "" ||
		health["last_write_at"] == nil {
		t.Fatalf("disabled retained health = %+v", health)
	}
}

func gatewayTelemetryEvent(index int) telemetry.EventV1 {
	startedAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	return telemetry.EventV1{
		SchemaVersion:     telemetry.SchemaVersionV1,
		Event:             telemetry.EventRequest,
		EventID:           formatTelemetryEventID(index),
		StartedAt:         startedAt,
		CompletedAt:       startedAt.Add(time.Second),
		DurationMS:        1000,
		TerminationReason: telemetry.TerminationCompleted,
		ActualCost:        telemetry.CostSnapshotV1{},
		Fallback:          telemetry.FallbackV1{},
		StrippedToolTypes: []string{},
	}
}

func formatTelemetryEventID(index int) string {
	const digits = "0123456789abcdef"
	value := make([]byte, 32)
	for position := len(value) - 1; position >= 0; position-- {
		value[position] = digits[index&15]
		index >>= 4
	}
	return string(value)
}

func telemetrySegmentCount(t *testing.T, dir string) int {
	t.Helper()
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(segments)
}

func telemetryLineCount(t *testing.T, dir string) int {
	t.Helper()
	segments, err := telemetry.DiscoverSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, segment := range segments {
		file, err := os.Open(segment.Path)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			count++
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			t.Fatal(scanErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	return count
}

func writeTelemetryGatewayConfig(
	t *testing.T,
	path string,
	dir string,
	enabled bool,
	retentionDays int,
) {
	t.Helper()
	body := fmt.Sprintf(
		"global:\n  routing_enabled: false\n  telemetry_dir: %s\n  telemetry_enabled: %t\n  telemetry_retention_days: %d\nclients: []\n",
		dir,
		enabled,
		retentionDays,
	)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
