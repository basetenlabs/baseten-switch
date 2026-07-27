package gateway

import (
	"fmt"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

type contextCompatibility struct {
	Known           bool
	Compatible      bool
	RequestedTokens int64
	TargetTokens    int64
}

func evaluateContextCompatibility(
	snapshot *pricing.Snapshot,
	provider string,
	model string,
	requestedTokens *int64,
) contextCompatibility {
	if snapshot == nil || requestedTokens == nil || *requestedTokens <= 0 {
		return contextCompatibility{}
	}
	record, ok := snapshot.Model(provider, model)
	if !ok || record.ContextTokens <= 0 {
		return contextCompatibility{}
	}
	return contextCompatibility{
		Known:           true,
		Compatible:      record.ContextTokens >= *requestedTokens,
		RequestedTokens: *requestedTokens,
		TargetTokens:    record.ContextTokens,
	}
}

func contextCompatibilityWarning(
	snapshot *pricing.Snapshot,
	provider string,
	model string,
	requestedTokens *int64,
) string {
	result := evaluateContextCompatibility(
		snapshot,
		provider,
		model,
		requestedTokens,
	)
	if !result.Known || result.Compatible {
		return ""
	}
	return fmt.Sprintf(
		"requested context budget %d exceeds known %s model %q limit %d",
		result.RequestedTokens,
		provider,
		model,
		result.TargetTokens,
	)
}
