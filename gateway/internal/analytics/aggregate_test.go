package analytics

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
	"github.com/basetenlabs/baseten-switch/gateway/internal/telemetry"
)

func TestBuildUsesPersistedCostsAndPreservesTrafficContract(t *testing.T) {
	claude := analyticsEvent(100, "anthropic", "claude-fable-5", "claude-fable-5")
	setUsage(&claude, 100, 100, 10, 20)
	setActualCost(&claude, 30_000_000_000)
	setLatency(&claude, 600, 1600)

	baseten := analyticsEvent(101, "baseten", "claude-opus-4-8", "zai-org/GLM-5.2")
	setUsage(&baseten, 1_000_000, 100_000, 20, 30)
	baseten.Usage.CacheWriteTotalInputTokens = int64Pointer(30)
	baseten.Usage.CacheWrite5mInputTokens = nil
	baseten.Usage.CacheWrite1hInputTokens = nil
	setActualCost(&baseten, 2_000_000_000)
	setCounterfactualCost(&baseten, 12_000_000_000)
	setLatency(&baseten, 200, 1200)

	unpricedCounterfactual := analyticsEvent(
		102,
		"baseten",
		"claude-sonnet-4-6",
		"moonshotai/Kimi-K3",
	)
	setUsage(&unpricedCounterfactual, 10, 5, 0, 0)
	setActualCost(&unpricedCounterfactual, 1_000_000_000)

	incomplete := analyticsEvent(
		103,
		"baseten",
		"claude-opus-4-8",
		"unknown-upstream",
	)
	incomplete.UsageComplete = false
	incomplete.Usage = telemetry.UsageV1{}

	events := []telemetry.EventV1{
		claude,
		baseten,
		unpricedCounterfactual,
		incomplete,
	}
	retained := Snapshot{
		Events: events, Complete: true,
		Earliest: claude.CompletedAt, Latest: incomplete.CompletedAt,
	}
	got := Build(events, Window{Since: 100, Until: 200}, 200, retained, true, nil)

	if got.Coverage.RequestRows != 4 ||
		got.Coverage.PricedActualCostRows != 3 ||
		got.Coverage.UnpricedActualCostRows != 1 ||
		got.Coverage.SavingsEligibleRows != 1 ||
		got.Coverage.SavingsUnpricedRows != 2 ||
		got.Coverage.UnpricedCounterfactualRows != 2 ||
		got.Coverage.IncompleteUsageRows != 1 ||
		!got.Coverage.CollectionEnabled {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	if got.Cost.Summary.ActualClaudeCostUSD != 30 ||
		got.Cost.Summary.ActualBasetenCostUSD != 3 ||
		got.Cost.Summary.EstimatedNativeCostForBasetenUSD != 12 ||
		got.Cost.Summary.SavedUSD != 10 {
		t.Fatalf("persisted cost summary = %+v", got.Cost.Summary)
	}
	if len(got.Cost.Providers) != 2 ||
		got.Cost.Providers[0].Provider != "Claude" ||
		got.Cost.Providers[1].Provider != "Baseten" {
		t.Fatalf("providers = %+v", got.Cost.Providers)
	}
	if got.Cost.Providers[1].Tokens != 1_100_065 {
		t.Fatalf("Baseten tokens = %d, want all token categories 1100065",
			got.Cost.Providers[1].Tokens)
	}
	if got.Cost.Providers[1].TokenBreakdown != (TokenBreakdown{
		InputTokens: 1_000_010, OutputTokens: 100_005,
		CacheReadInputTokens: 20, CacheWriteInputTokens: 30, CompleteRequests: 2,
	}) {
		t.Fatalf("Baseten token breakdown = %+v", got.Cost.Providers[1].TokenBreakdown)
	}
	if len(got.Cost.Savings.ByBasetenModel) != 1 ||
		got.Cost.Savings.ByBasetenModel[0].ModelID != "zai-org/GLM-5.2" ||
		got.Cost.Savings.ByBasetenModel[0].DisplayName != "GLM 5.2" {
		t.Fatalf("savings models = %+v", got.Cost.Savings.ByBasetenModel)
	}
	if got.Cost.Models[0].ModelID != "fable" ||
		got.Cost.Models[0].DisplayName != "Fable" {
		t.Fatalf("Claude model metadata = %+v", got.Cost.Models[0])
	}
	if got.Performance.Providers[0].MedianTTFTMs != 600 ||
		got.Performance.Providers[1].MedianTTFTMs != 200 {
		t.Fatalf("performance = %+v", got.Performance.Providers)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"model":`) ||
		strings.Contains(string(encoded), `"baseten_model":`) {
		t.Fatalf("response exposes retired model field: %s", encoded)
	}
}

func TestBuildIncludesCacheTokensWithoutChangingCostOrOutputSpeed(t *testing.T) {
	for _, test := range []struct {
		name       string
		cacheRead  int64
		write5m    int64
		write1h    int64
		totalOnly  bool
		classified bool
		wantTokens int64
	}{
		{name: "no cache", wantTokens: 2_000},
		{name: "cache reads", cacheRead: 1_000_000, wantTokens: 1_002_000},
		{name: "five minute writes", write5m: 1_000_000, wantTokens: 1_002_000},
		{name: "one hour writes", write1h: 1_000_000, wantTokens: 1_002_000},
		{name: "all cache categories", cacheRead: 1_000_000, write5m: 1_000_000, write1h: 1_000_000, wantTokens: 3_002_000},
		{name: "total only writes", write5m: 1_000_000, write1h: 1_000_000, totalOnly: true, wantTokens: 2_002_000},
		{name: "permission check", cacheRead: 1_000_000, classified: true, wantTokens: 1_002_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := analyticsEvent(100, "anthropic", "claude-sonnet-4-6", "claude-sonnet-4-6")
			setUsage(&event, 1_000, 1_000, test.cacheRead, test.write5m)
			event.Usage.CacheWrite1hInputTokens = int64Pointer(test.write1h)
			if test.totalOnly {
				event.Usage.CacheWriteTotalInputTokens = int64Pointer(test.write5m + test.write1h)
				event.Usage.CacheWrite5mInputTokens = nil
				event.Usage.CacheWrite1hInputTokens = nil
			}
			if test.classified {
				event.RequestClassification = &telemetry.RequestClassificationV1{
					Kind:          telemetry.RequestClassificationKindClaudeAutoPermissionCheck,
					Detector:      telemetry.RequestClassificationDetectorClaudeAutoV1,
					RoutingAction: telemetry.RequestClassificationRoutingActionNativeAnthropic,
				}
			}
			setActualCost(&event, 103_000_000)
			setLatency(&event, 200, 1200)
			got := Build([]telemetry.EventV1{event}, Window{Since: 100, Until: 200}, 200, Snapshot{Complete: true}, true, nil)
			want := TokenBreakdown{
				InputTokens: 1_000, OutputTokens: 1_000,
				CacheReadInputTokens: test.cacheRead, CacheWriteInputTokens: test.write5m + test.write1h,
				CompleteRequests: 1,
			}
			for _, group := range append(got.Cost.Providers, got.Cost.Models...) {
				if group.Tokens != test.wantTokens || group.TokenBreakdown != want || group.Requests != 1 {
					t.Errorf("cost token group = %+v, want %d tokens, %+v", group, test.wantTokens, want)
				}
				if group.ActualCostUSD == nil || *group.ActualCostUSD != 0.103 {
					t.Errorf("persisted cost changed: %+v", group)
				}
			}
			for _, group := range append(got.Performance.Providers, got.Performance.Models...) {
				if group.Tokens != test.wantTokens || group.TokenBreakdown != want || group.Requests != 1 {
					t.Errorf("performance token group = %+v, want %d tokens, %+v", group, test.wantTokens, want)
				}
				if group.TTFTSamples != 1 || group.MedianTTFTMs != 200 || group.OutputTPSSamples != 1 || group.MedianOutputTokensPerSecond != 1_000 {
					t.Errorf("timing changed: %+v", group)
				}
			}
		})
	}
}

func TestBuildTokenCoverageIsIndependentOfPricing(t *testing.T) {
	complete := analyticsEvent(100, "baseten", "claude-sonnet-4-6", "example/model")
	setUsage(&complete, 100, 50, 1_000, 200)
	setLatency(&complete, 200, 1200)
	setActualCost(&complete, 103_000_000)
	incomplete := complete
	incomplete.UsageComplete = false
	incomplete.ActualCost = telemetry.CostSnapshotV1{}
	unpriced := complete
	unpriced.ActualCost = telemetry.CostSnapshotV1{}
	unknown := incomplete
	unknown.Usage = telemetry.UsageV1{}
	zero := complete
	setUsage(&zero, 0, 0, 0, 0)
	setActualCost(&zero, 0)

	for _, test := range []struct {
		name          string
		events        []telemetry.EventV1
		wantTokens    int64
		wantBreakdown TokenBreakdown
		wantPriced    int
	}{
		{
			name: "partial usage", events: []telemetry.EventV1{complete, incomplete, unknown}, wantTokens: 1_350,
			wantBreakdown: TokenBreakdown{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 1_000, CacheWriteInputTokens: 200, CompleteRequests: 1},
			wantPriced:    1,
		},
		{
			name: "complete unpriced", events: []telemetry.EventV1{unpriced}, wantTokens: 1_350,
			wantBreakdown: TokenBreakdown{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 1_000, CacheWriteInputTokens: 200, CompleteRequests: 1},
		},
		{name: "all unknown", events: []telemetry.EventV1{unknown}},
		{name: "reported zero", events: []telemetry.EventV1{zero}, wantBreakdown: TokenBreakdown{CompleteRequests: 1}, wantPriced: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Build(test.events, Window{Since: 100, Until: 200}, 200, Snapshot{Complete: true}, true, nil)
			for _, group := range append(got.Cost.Providers, got.Cost.Models...) {
				if group.Tokens != test.wantTokens || group.TokenBreakdown != test.wantBreakdown || group.Requests != len(test.events) {
					t.Errorf("cost group = %+v", group)
				}
				if group.PricedRows != test.wantPriced || group.UnpricedRows != len(test.events)-test.wantPriced || (group.ActualCostUSD == nil) != (test.wantPriced == 0) {
					t.Errorf("pricing coverage changed: %+v", group)
				}
			}
			for _, group := range append(got.Performance.Providers, got.Performance.Models...) {
				if group.Tokens != test.wantTokens || group.TokenBreakdown != test.wantBreakdown || group.Requests != len(test.events) || group.TTFTSamples != test.wantBreakdown.CompleteRequests {
					t.Errorf("performance group = %+v", group)
				}
			}
			if got.Coverage.IncompleteUsageRows != len(test.events)-test.wantBreakdown.CompleteRequests {
				t.Errorf("incomplete usage rows = %d", got.Coverage.IncompleteUsageRows)
			}
		})
	}
}

func TestBuildTokenGroupsRespectWindow(t *testing.T) {
	var events []telemetry.EventV1
	for index, completedAt := range []int64{99, 100, 101, 200} {
		event := analyticsEvent(completedAt, "baseten", "claude-sonnet-4-6", fmt.Sprintf("example/model-%d", index))
		setUsage(&event, 10, 20, 30, 40)
		events = append(events, event)
	}
	got := Build(events, Window{Since: 100, Until: 200}, 200, Snapshot{Complete: true}, true, nil)
	want := TokenBreakdown{InputTokens: 20, OutputTokens: 40, CacheReadInputTokens: 60, CacheWriteInputTokens: 80, CompleteRequests: 2}
	if got.Coverage.RequestRows != 2 || len(got.Cost.Models) != 2 || len(got.Performance.Models) != 2 || len(got.Cost.Providers) != 1 || len(got.Performance.Providers) != 1 {
		t.Fatalf("window groups = %+v", got)
	}
	if got.Cost.Providers[0].Tokens != 200 || got.Cost.Providers[0].TokenBreakdown != want || got.Performance.Providers[0].Tokens != 200 || got.Performance.Providers[0].TokenBreakdown != want {
		t.Fatalf("provider totals = %+v / %+v", got.Cost.Providers, got.Performance.Providers)
	}
	for index, cost := range got.Cost.Models {
		perf := got.Performance.Models[index]
		if cost.ModelID != fmt.Sprintf("example/model-%d", index+1) || perf.ModelID != cost.ModelID || cost.Tokens != 100 || perf.Tokens != 100 || cost.TokenBreakdown != perf.TokenBreakdown || cost.TokenBreakdown != (TokenBreakdown{InputTokens: 10, OutputTokens: 20, CacheReadInputTokens: 30, CacheWriteInputTokens: 40, CompleteRequests: 1}) {
			t.Errorf("model totals = %+v / %+v", cost, perf)
		}
	}
}

func TestBuildGroupsByIdentityNotDisplayName(t *testing.T) {
	claudeOne := analyticsEvent(100, "anthropic", "claude-sonnet-4-6", "claude-sonnet-4-6")
	claudeTwo := analyticsEvent(101, "anthropic", "claude-sonnet-5-0", "claude-sonnet-5-0")
	basetenOne := analyticsEvent(102, "baseten", "claude-opus-4-8", "org-a/shared-model")
	basetenTwo := analyticsEvent(103, "baseten", "claude-opus-4-8", "org-b/shared_model")
	events := []telemetry.EventV1{claudeOne, claudeTwo, basetenOne, basetenTwo}
	for index := range events {
		setUsage(&events[index], 1, 1, 0, 0)
	}

	got := Build(
		events,
		Window{Since: 100, Until: 200},
		200,
		Snapshot{Events: events, Complete: true},
		true,
		nil,
	)
	if len(got.Cost.Models) != 3 {
		t.Fatalf("model groups = %+v, want one Claude family and two Baseten IDs", got.Cost.Models)
	}
	if got.Cost.Models[0].ModelID != "sonnet" ||
		got.Cost.Models[0].DisplayName != "Sonnet" ||
		got.Cost.Models[0].Requests != 2 {
		t.Fatalf("Claude family group = %+v", got.Cost.Models[0])
	}
	if got.Cost.Models[1].ModelID != "org-a/shared-model" ||
		got.Cost.Models[2].ModelID != "org-b/shared_model" ||
		got.Cost.Models[1].DisplayName != "shared model" ||
		got.Cost.Models[2].DisplayName != "shared model" {
		t.Fatalf("Baseten identity groups = %+v", got.Cost.Models[1:])
	}
}

func TestBuildProjectsCatalogNameAcrossTraffic(t *testing.T) {
	event := analyticsEvent(
		100,
		"baseten",
		"claude-opus-4-8",
		"baseten/inkling-v1",
	)
	setUsage(&event, 100, 20, 0, 0)
	setActualCost(&event, 2_000_000_000)
	setCounterfactualCost(&event, 10_000_000_000)
	setLatency(&event, 200, 1200)

	catalog := pricing.New()
	if err := catalog.ReplaceProviderAvailability(
		pricing.ProviderBaseten,
		[]pricing.AvailabilityModel{{
			CanonicalModelID: "baseten/inkling-v1",
			DisplayName:      "Inkling",
		}},
		"test_model_apis",
		time.Unix(90, 0).UTC(),
		"inkling-v1",
	); err != nil {
		t.Fatal(err)
	}
	retained := Snapshot{Events: []telemetry.EventV1{event}, Complete: true}
	got := Build(
		retained.Events,
		Window{Since: 90, Until: 110},
		110,
		retained,
		true,
		catalog.Capture(),
	)

	if len(got.Cost.Models) != 1 ||
		got.Cost.Models[0].ModelID != "baseten/inkling-v1" ||
		got.Cost.Models[0].DisplayName != "Inkling" {
		t.Fatalf("cost models = %+v", got.Cost.Models)
	}
	if len(got.Performance.Models) != 1 ||
		got.Performance.Models[0].ModelID != "baseten/inkling-v1" ||
		got.Performance.Models[0].DisplayName != "Inkling" {
		t.Fatalf("performance models = %+v", got.Performance.Models)
	}
	if len(got.Cost.Savings.ByBasetenModel) != 1 ||
		got.Cost.Savings.ByBasetenModel[0].ModelID != "baseten/inkling-v1" ||
		got.Cost.Savings.ByBasetenModel[0].DisplayName != "Inkling" {
		t.Fatalf("savings models = %+v", got.Cost.Savings.ByBasetenModel)
	}
	if len(got.Cost.Savings.Mappings) != 1 ||
		got.Cost.Savings.Mappings[0].BasetenModelID != "baseten/inkling-v1" ||
		got.Cost.Savings.Mappings[0].BasetenDisplayName != "Inkling" {
		t.Fatalf("savings mappings = %+v", got.Cost.Savings.Mappings)
	}
}

func TestBuildCollectionDisabledRetainsHistory(t *testing.T) {
	event := analyticsEvent(10, "baseten", "claude-opus-4-8", "zai-org/GLM-5.2")
	setUsage(&event, 1, 1, 0, 0)
	setActualCost(&event, 2_000_000_000)
	setCounterfactualCost(&event, 10_000_000_000)
	retained := Snapshot{
		Events:   []telemetry.EventV1{event},
		Complete: true,
		Earliest: event.CompletedAt,
		Latest:   event.CompletedAt,
	}
	got := Build(
		retained.Events,
		Window{Since: 1, Until: 20},
		20,
		retained,
		false,
		nil,
	)
	if got.Coverage.CollectionEnabled {
		t.Fatal("collection_enabled = true")
	}
	if got.Coverage.RequestRows != 1 ||
		got.Cost.Summary.ActualBasetenCostUSD != 2 ||
		got.Cost.Summary.SavedUSD != 8 {
		t.Fatalf("disabled collection hid retained history: %+v", got)
	}
}

func TestBuildEmptySlicesEncodeAsArrays(t *testing.T) {
	got := Build(
		nil,
		Window{Since: 1, Until: 2},
		2,
		Snapshot{Complete: true},
		true,
		nil,
	)
	if got.Cost.Providers == nil || got.Cost.Models == nil ||
		got.Cost.Savings.ByBasetenModel == nil || got.Cost.Savings.Mappings == nil ||
		got.Performance.Providers == nil || got.Performance.Models == nil {
		t.Fatalf("nil collection in empty response: %+v", got)
	}
}

func analyticsEvent(
	unix int64,
	provider string,
	requested string,
	served string,
) telemetry.EventV1 {
	started := time.Unix(unix-1, 0).UTC()
	completed := time.Unix(unix, 0).UTC()
	status := 200
	return telemetry.EventV1{
		SchemaVersion:        telemetry.SchemaVersionV1,
		Event:                telemetry.EventRequest,
		EventID:              fmt.Sprintf("%032x", unix),
		StartedAt:            started,
		CompletedAt:          completed,
		Client:               "claude-code",
		ConfiguredRoute:      provider,
		EffectiveProvider:    provider,
		RequestedModel:       requested,
		RequestedModelFamily: "",
		ModelFamilyRevision:  "test-v1",
		ServedModel:          served,
		Status:               &status,
		DurationMS:           1000,
		TerminationReason:    telemetry.TerminationCompleted,
		ActualCost:           telemetry.CostSnapshotV1{},
		Fallback:             telemetry.FallbackV1{},
		StrippedToolTypes:    []string{},
	}
}

func setUsage(event *telemetry.EventV1, in, out, cacheRead, cacheWrite int64) {
	event.UsageComplete = true
	event.Usage = telemetry.UsageV1{
		InputTokens:             int64Pointer(in),
		OutputTokens:            int64Pointer(out),
		CacheReadInputTokens:    int64Pointer(cacheRead),
		CacheWrite5mInputTokens: int64Pointer(cacheWrite),
		CacheWrite1hInputTokens: int64Pointer(0),
	}
}

func setActualCost(event *telemetry.EventV1, nanoUSD int64) {
	event.ActualCost = pricedCost(nanoUSD, event.StartedAt)
}

func setCounterfactualCost(event *telemetry.EventV1, nanoUSD int64) {
	value := pricedCost(nanoUSD, event.StartedAt)
	event.NativeCounterfactualCost = &value
}

func pricedCost(nanoUSD int64, capturedAt time.Time) telemetry.CostSnapshotV1 {
	revision := "test-revision"
	return telemetry.CostSnapshotV1{
		Priced:     true,
		NanoUSD:    int64Pointer(nanoUSD),
		Source:     "test",
		Revision:   &revision,
		CapturedAt: &capturedAt,
		RatesNanoUSDPerToken: &telemetry.TokenRatesV1{
			Input:             int64Pointer(1),
			Output:            int64Pointer(1),
			CacheReadInput:    int64Pointer(1),
			CacheWrite5mInput: int64Pointer(1),
			CacheWrite1hInput: int64Pointer(1),
		},
	}
}

func setLatency(event *telemetry.EventV1, ttft, duration int64) {
	event.TTFTMS = int64Pointer(ttft)
	event.DurationMS = duration
}

func int64Pointer(value int64) *int64 {
	return &value
}
