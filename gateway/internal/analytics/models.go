package analytics

type Window struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
}

type Coverage struct {
	RequestRows                int    `json:"request_rows"`
	PricedActualCostRows       int    `json:"priced_actual_cost_rows"`
	UnpricedActualCostRows     int    `json:"unpriced_actual_cost_rows"`
	SavingsEligibleRows        int    `json:"savings_eligible_rows"`
	SavingsUnpricedRows        int    `json:"savings_unpriced_rows"`
	IncompleteUsageRows        int    `json:"incomplete_usage_rows"`
	UnpricedCounterfactualRows int    `json:"unpriced_counterfactual_rows"`
	CollectionEnabled          bool   `json:"collection_enabled"`
	EarliestCompletedAt        *int64 `json:"earliest_completed_at"`
	LatestCompletedAt          *int64 `json:"latest_completed_at"`
	Complete                   bool   `json:"complete"`
	Reason                     string `json:"reason"`
}

type CostSummary struct {
	ActualClaudeCostUSD              float64 `json:"actual_claude_cost_usd"`
	ActualBasetenCostUSD             float64 `json:"actual_baseten_cost_usd"`
	EstimatedNativeCostForBasetenUSD float64 `json:"estimated_native_cost_for_baseten_usd"`
	SavedUSD                         float64 `json:"saved_usd"`
	SavedPercent                     float64 `json:"saved_percent"`
}

type CostGroup struct {
	Provider      string   `json:"provider"`
	ModelID       string   `json:"model_id,omitempty"`
	DisplayName   string   `json:"display_name,omitempty"`
	Requests      int      `json:"requests"`
	Tokens        int64    `json:"tokens"`
	ActualCostUSD *float64 `json:"actual_cost_usd"`
	PricedRows    int      `json:"priced_rows"`
	UnpricedRows  int      `json:"unpriced_rows"`
}

type SavingsModel struct {
	ModelID                string  `json:"model_id"`
	DisplayName            string  `json:"display_name"`
	ActualBasetenCostUSD   float64 `json:"actual_baseten_cost_usd"`
	EstimatedNativeCostUSD float64 `json:"estimated_native_cost_usd"`
	SavedUSD               float64 `json:"saved_usd"`
	SavedPercent           float64 `json:"saved_percent"`
}

type SavingsMapping struct {
	BasetenModelID         string  `json:"baseten_model_id"`
	BasetenDisplayName     string  `json:"baseten_display_name"`
	RequestedClaudeFamily  string  `json:"requested_claude_family"`
	ActualBasetenCostUSD   float64 `json:"actual_baseten_cost_usd"`
	EstimatedNativeCostUSD float64 `json:"estimated_native_cost_usd"`
}

type Savings struct {
	ByBasetenModel []SavingsModel   `json:"by_baseten_model"`
	Mappings       []SavingsMapping `json:"mappings"`
}

type Cost struct {
	Summary   CostSummary `json:"summary"`
	Providers []CostGroup `json:"providers"`
	Models    []CostGroup `json:"models"`
	Savings   Savings     `json:"savings"`
}

type PerformanceGroup struct {
	Provider                    string  `json:"provider"`
	ModelID                     string  `json:"model_id,omitempty"`
	DisplayName                 string  `json:"display_name,omitempty"`
	Requests                    int     `json:"requests"`
	Tokens                      int64   `json:"tokens"`
	TTFTSamples                 int     `json:"ttft_samples"`
	MedianTTFTMs                int64   `json:"median_ttft_ms"`
	OutputTPSSamples            int     `json:"output_tps_samples"`
	MedianOutputTokensPerSecond float64 `json:"median_output_tokens_per_second"`
}

type Performance struct {
	Providers []PerformanceGroup `json:"providers"`
	Models    []PerformanceGroup `json:"models"`
}

type Response struct {
	GeneratedAt int64       `json:"generated_at"`
	Window      Window      `json:"window"`
	Coverage    Coverage    `json:"coverage"`
	Cost        Cost        `json:"cost"`
	Performance Performance `json:"performance"`
}
