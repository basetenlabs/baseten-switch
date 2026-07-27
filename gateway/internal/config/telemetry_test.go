package config

import (
	"strings"
	"testing"
)

func TestIsTelemetryEnabledDefaultsTrue(t *testing.T) {
	if !IsTelemetryEnabled(Global{}) {
		t.Fatal("omitted telemetry_enabled should default to true")
	}

	enabled := true
	if !IsTelemetryEnabled(Global{TelemetryEnabled: &enabled}) {
		t.Fatal("explicit telemetry_enabled=true should enable collection")
	}

	enabled = false
	if IsTelemetryEnabled(Global{TelemetryEnabled: &enabled}) {
		t.Fatal("explicit telemetry_enabled=false should disable collection")
	}
}

func TestTelemetryRetentionDaysDefaultsToNinety(t *testing.T) {
	if got := TelemetryRetentionDays(Global{}); got != 90 {
		t.Fatalf("TelemetryRetentionDays() = %d, want 90", got)
	}
	if got := TelemetryRetentionDays(Global{TelemetryRetentionDays: 30}); got != 30 {
		t.Fatalf("TelemetryRetentionDays() = %d, want 30", got)
	}
}

func TestValidateRoutingPolicyRejectsNegativeTelemetryRetention(t *testing.T) {
	enabled := false
	err := ValidateRoutingPolicy(&File{Global: Global{
		RoutingEnabled:         &enabled,
		TelemetryRetentionDays: -1,
	}})
	if err == nil || !strings.Contains(err.Error(), "telemetry_retention_days") {
		t.Fatalf("ValidateRoutingPolicy() error = %v", err)
	}
}
