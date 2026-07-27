package gateway

import (
	"fmt"
	"os"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

// writeTelemetryV1 is deliberately best-effort. Request routing must never
// fail because the local telemetry store could not open or append an event.
func (g *Gateway) writeTelemetryV1(event telemetry.EventV1) {
	g.telemetryMu.Lock()
	defer g.telemetryMu.Unlock()
	if g.telemetryStopped {
		return
	}

	runtimeCfg := g.runtimeConfig()
	if runtimeCfg.TelemetryEnabled != nil && !*runtimeCfg.TelemetryEnabled {
		return
	}
	dir, retentionDays := telemetryWriterSettings(runtimeCfg)
	if g.telemetryWriter != nil &&
		(g.telemetryWriterDir != dir || g.telemetryRetentionDays != retentionDays) {
		g.closeActiveTelemetryWriterLocked()
	}
	if g.telemetryWriter == nil {
		writer, err := telemetry.NewWriter(telemetry.WriterOptions{
			Dir:           dir,
			RetentionDays: retentionDays,
		})
		if err != nil {
			g.telemetryLastHealth.LastWriteError = err.Error()
			g.telemetryLastHealth.DroppedEvents++
			g.telemetryLastHealth.RetentionDays = retentionDays
			g.telemetryLastDir = dir
			fmt.Fprintf(os.Stderr, "[gateway] telemetry writer failed: %v\n", err)
			return
		}
		g.telemetryWriter = writer
		g.telemetryWriterDir = dir
		g.telemetryRetentionDays = retentionDays
	}
	if err := g.telemetryWriter.Write(event); err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] telemetry write failed: %v\n", err)
	}
}

// reconcileTelemetryWriter eagerly closes a writer made obsolete by reload.
// Enabling remains lazy: the new writer opens on the next submitted event.
func (g *Gateway) reconcileTelemetryWriter(runtimeCfg Config) {
	g.telemetryMu.Lock()
	defer g.telemetryMu.Unlock()
	if g.telemetryWriter == nil {
		return
	}
	dir, retentionDays := telemetryWriterSettings(runtimeCfg)
	disabled := runtimeCfg.TelemetryEnabled != nil && !*runtimeCfg.TelemetryEnabled
	if disabled ||
		g.telemetryWriterDir != dir ||
		g.telemetryRetentionDays != retentionDays {
		g.closeActiveTelemetryWriterLocked()
	}
}

func telemetryWriterSettings(runtimeCfg Config) (string, int) {
	dir := runtimeCfg.TelemetryDir
	if dir == "" {
		dir = config.DefaultTelemetryDir()
	}
	retentionDays := runtimeCfg.TelemetryRetentionDays
	if retentionDays <= 0 {
		retentionDays = config.DefaultTelemetryRetentionDays
	}
	return dir, retentionDays
}

func (g *Gateway) closeActiveTelemetryWriterLocked() {
	if g.telemetryWriter == nil {
		return
	}
	g.telemetryLastHealth = g.telemetryWriter.Health()
	g.telemetryLastDir = g.telemetryWriterDir
	if err := g.telemetryWriter.Close(); err != nil {
		g.telemetryLastHealth.LastWriteError = err.Error()
		fmt.Fprintf(os.Stderr, "[gateway] telemetry close failed: %v\n", err)
	}
	g.telemetryWriter = nil
	g.telemetryWriterDir = ""
	g.telemetryRetentionDays = 0
}

func (g *Gateway) closeTelemetryWriter() error {
	g.telemetryMu.Lock()
	defer g.telemetryMu.Unlock()
	g.telemetryStopped = true
	if g.telemetryWriter == nil {
		return nil
	}
	g.telemetryLastHealth = g.telemetryWriter.Health()
	g.telemetryLastDir = g.telemetryWriterDir
	err := g.telemetryWriter.Close()
	if err != nil {
		g.telemetryLastHealth.LastWriteError = err.Error()
	}
	g.telemetryWriter = nil
	g.telemetryWriterDir = ""
	g.telemetryRetentionDays = 0
	return err
}

func (g *Gateway) telemetryAdminHealth(runtimeCfg Config) map[string]any {
	g.telemetryMu.Lock()
	defer g.telemetryMu.Unlock()

	dir, retentionDays := telemetryWriterSettings(runtimeCfg)
	enabled := runtimeCfg.TelemetryEnabled == nil || *runtimeCfg.TelemetryEnabled
	health := telemetry.WriterHealth{RetentionDays: retentionDays}
	if g.telemetryWriter != nil &&
		g.telemetryWriterDir == dir &&
		g.telemetryRetentionDays == retentionDays {
		health = g.telemetryWriter.Health()
	} else if g.telemetryLastDir == dir {
		health = g.telemetryLastHealth
		health.RetentionDays = retentionDays
	}

	var lastWriteAt any
	if health.LastWriteAt != nil {
		lastWriteAt = health.LastWriteAt.UTC().Format(time.RFC3339Nano)
	}
	var lastWriteError any
	if health.LastWriteError != "" {
		lastWriteError = health.LastWriteError
	}
	var lastRetentionError any
	if health.LastRetentionError != "" {
		lastRetentionError = health.LastRetentionError
	}
	return map[string]any{
		"collection_enabled":      enabled,
		"active_segment":          health.ActiveSegment,
		"last_write_at":           lastWriteAt,
		"last_write_error":        lastWriteError,
		"dropped_events":          health.DroppedEvents,
		"recovered_partial_lines": health.RecoveredPartialLines,
		"retention_days":          health.RetentionDays,
		"last_retention_error":    lastRetentionError,
	}
}
