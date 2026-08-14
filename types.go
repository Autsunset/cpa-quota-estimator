package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const (
	pluginID      = "cpa-quota-estimator"
	pluginName    = "CPA Quota Estimator"
	pluginVersion = "0.1.0"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The ABI serializes SDK structs with their exported Go field names.
type usageRecord struct {
	Provider        string       `json:"Provider"`
	ExecutorType    string       `json:"ExecutorType"`
	Model           string       `json:"Model"`
	Alias           string       `json:"Alias"`
	APIKey          string       `json:"APIKey"`
	AuthID          string       `json:"AuthID"`
	AuthIndex       string       `json:"AuthIndex"`
	AuthType        string       `json:"AuthType"`
	Source          string       `json:"Source"`
	ReasoningEffort string       `json:"ReasoningEffort"`
	ServiceTier     string       `json:"ServiceTier"`
	Generate        bool         `json:"Generate"`
	RequestedAt     time.Time    `json:"RequestedAt"`
	Latency         int64        `json:"Latency"`
	TTFT            int64        `json:"TTFT"`
	Failed          bool         `json:"Failed"`
	Failure         usageFailure `json:"Failure"`
	Detail          usageDetail  `json:"Detail"`
	ResponseHeaders http.Header  `json:"ResponseHeaders"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

type config struct {
	Enabled                  bool    `yaml:"enabled"`
	DataPath                 string  `yaml:"data_path"`
	SampleIntervalMinutes    int     `yaml:"sample_interval_minutes"`
	PriceSourceURL           string  `yaml:"price_source_url"`
	PriceSyncIntervalMinutes int     `yaml:"price_sync_interval_minutes"`
	FastPricingMode          string  `yaml:"fast_pricing_mode"`
	FastMultiplier           float64 `yaml:"fast_multiplier"`
	LongContextThreshold     int64   `yaml:"long_context_threshold"`
	HistoryDays              int     `yaml:"history_days"`
}

func defaultConfig() config {
	return config{
		Enabled:                  true,
		DataPath:                 "/CLIProxyAPI/data/cpa-quota-estimator.sqlite",
		SampleIntervalMinutes:    5,
		PriceSourceURL:           "https://models.dev/catalog.json",
		PriceSyncIntervalMinutes: 1440,
		FastPricingMode:          "multiplier",
		FastMultiplier:           2.5,
		LongContextThreshold:     272000,
		HistoryDays:              180,
	}
}

type price struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	LongInput  float64 `json:"long_input"`
	LongOutput float64 `json:"long_output"`
	LongRead   float64 `json:"long_cache_read"`
	LongWrite  float64 `json:"long_cache_write"`
	FastInput  float64 `json:"fast_input"`
	FastOutput float64 `json:"fast_output"`
	FastRead   float64 `json:"fast_cache_read"`
	FastWrite  float64 `json:"fast_cache_write"`
	Source     string  `json:"source"`
	UpdatedAt  int64   `json:"updated_at"`
}

type event struct {
	RequestedAt      int64
	Account          string
	Provider         string
	Model            string
	Alias            string
	ServiceTier      string
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	CostUSD          float64
	Failed           bool
	StatusCode       int
	UsedPercent      *float64
	ResetAt          int64
	WindowMinutes    int64
	PlanType         string
}

type quotaPoint struct {
	Time          int64   `json:"time"`
	UsedPercent   float64 `json:"used_percent"`
	ResetAt       int64   `json:"reset_at"`
	WindowMinutes int64   `json:"window_minutes"`
	WindowTokens  int64   `json:"window_tokens"`
	WindowCostUSD float64 `json:"window_cost_usd"`
	Requests      int64   `json:"requests"`
}

type estimate struct {
	Available          bool    `json:"available"`
	PercentSpan        float64 `json:"percent_span"`
	SampleCount        int     `json:"sample_count"`
	FullWindowTokens   float64 `json:"full_window_tokens"`
	FullWindowCostUSD  float64 `json:"full_window_cost_usd"`
	TokenLow           float64 `json:"token_low"`
	TokenHigh          float64 `json:"token_high"`
	CostLow            float64 `json:"cost_low"`
	CostHigh           float64 `json:"cost_high"`
	RemainingTokens    float64 `json:"remaining_tokens"`
	RemainingCostUSD   float64 `json:"remaining_cost_usd"`
	EstimatedExhaustAt int64   `json:"estimated_exhaust_at,omitempty"`
	Confidence         string  `json:"confidence"`
	Explanation        string  `json:"explanation"`
}

type capacityPoint struct {
	Time              int64   `json:"time"`
	UsedPercent       float64 `json:"used_percent"`
	FullWindowTokens  float64 `json:"full_window_tokens"`
	FullWindowCostUSD float64 `json:"full_window_cost_usd"`
	SampleCount       int     `json:"sample_count"`
}

type burnForecast struct {
	Available                bool    `json:"available"`
	WindowStart              int64   `json:"window_start"`
	ResetAt                  int64   `json:"reset_at"`
	CalculatedAt             int64   `json:"calculated_at"`
	WindowSeconds            int64   `json:"window_seconds"`
	ElapsedSeconds           int64   `json:"elapsed_seconds"`
	RemainingSeconds         int64   `json:"remaining_seconds"`
	TimeProgressPercent      float64 `json:"time_progress_percent"`
	UsedPercent              float64 `json:"used_percent"`
	ExpectedUsedPercent      float64 `json:"expected_used_percent"`
	PaceDeltaPercent         float64 `json:"pace_delta_percent"`
	PaceRatio                float64 `json:"pace_ratio"`
	AveragePercentPerDay     float64 `json:"average_percent_per_day"`
	SustainablePercentPerDay float64 `json:"sustainable_percent_per_day"`
	ProjectedUsedAtReset     float64 `json:"projected_used_at_reset"`
	EstimatedExhaustAt       int64   `json:"estimated_exhaust_at,omitempty"`
	WillExhaustBeforeReset   bool    `json:"will_exhaust_before_reset"`
	RecentAvailable          bool    `json:"recent_available"`
	RecentWindowSeconds      int64   `json:"recent_window_seconds"`
	RecentPercentSpan        float64 `json:"recent_percent_span"`
	RecentPercentPerDay      float64 `json:"recent_percent_per_day"`
	RecentProjectedAtReset   float64 `json:"recent_projected_at_reset"`
	RecentEstimatedExhaustAt int64   `json:"recent_estimated_exhaust_at,omitempty"`
	RecentWillExhaustBefore  bool    `json:"recent_will_exhaust_before_reset"`
	Status                   string  `json:"status"`
}
