package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func TestSpendReadsV1SegmentsAndPersistedActualCost(t *testing.T) {
	root := t.TempDir()
	telemetryDir := filepath.Join(root, "telemetry")
	configPath := filepath.Join(root, "gateway.yaml")
	if err := config.Save(configPath, &config.File{
		Global: config.Global{TelemetryDir: telemetryDir},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASETEN_SWITCH_CONFIG_PATH", configPath)

	completedAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	event := doctorTestTelemetryEvent(
		completedAt,
		1,
		"claude-code",
		"baseten",
		"baseten",
		"claude-opus-4-8",
		500,
		false,
	)
	event.ServedModel = "zai-org/GLM-5.2"
	input := int64(10)
	output := int64(2)
	cacheRead := int64(3)
	cacheWriteTotal := int64(77)
	event.UsageComplete = true
	event.Usage = telemetry.UsageV1{
		InputTokens:                &input,
		OutputTokens:               &output,
		CacheReadInputTokens:       &cacheRead,
		CacheWriteTotalInputTokens: &cacheWriteTotal,
	}
	nanoUSD := int64(1_250_000_000)
	rate := int64(1)
	revision := "persisted-test"
	event.ActualCost = telemetry.CostSnapshotV1{
		Priced:     true,
		NanoUSD:    &nanoUSD,
		Source:     "test",
		Revision:   &revision,
		CapturedAt: &completedAt,
		RatesNanoUSDPerToken: &telemetry.TokenRatesV1{
			Input:             &rate,
			Output:            &rate,
			CacheReadInput:    &rate,
			CacheWrite5mInput: &rate,
			CacheWrite1hInput: &rate,
		},
		RateProvenance: spendTestRateProvenance(completedAt),
	}
	if err := writeDoctorTelemetryEvents(telemetryDir, event); err != nil {
		t.Fatal(err)
	}

	outputText := captureSpendOutput(t, func() int { return cmdSpend(nil) })
	for _, want := range []string{
		"2026-07-25",
		"baseten",
		"zai-org/GLM-5.2",
		"10",
		"cache_5m",
		"cache_1h",
		"cache_tot",
		"77",
		"1.250000",
	} {
		if !strings.Contains(outputText, want) {
			t.Fatalf("spend output missing %q:\n%s", want, outputText)
		}
	}
}

func spendTestRateProvenance(
	capturedAt time.Time,
) map[string]telemetry.RateProvenanceV1 {
	value := telemetry.RateProvenanceV1{
		Source:     "test",
		LoadedFrom: "live",
		Revision:   "persisted-test",
		CapturedAt: capturedAt,
	}
	return map[string]telemetry.RateProvenanceV1{
		"input":          value,
		"output":         value,
		"cache_read":     value,
		"cache_write_5m": value,
		"cache_write_1h": value,
	}
}

func captureSpendOutput(t *testing.T, run func() int) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	code := run()
	_ = writer.Close()
	os.Stdout = original
	output, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("cmdSpend exit = %d", code)
	}
	return string(output)
}
