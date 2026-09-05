package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const (
	pluginID   = "cpa-quota-estimator"
	pluginName = "CPA Quota Estimator"
)

var pluginVersion = "0.10.1"

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
	ApplyModelCalibration    bool    `yaml:"apply_model_calibration"`
	AstraMultiplier          float64 `yaml:"astra_multiplier"`
	Enabled                  bool    `yaml:"enabled"`
	DataPath                 string  `yaml:"data_path"`
	SampleIntervalMinutes    int     `yaml:"sample_interval_minutes"`
	PriceSourceURL           string  `yaml:"price_source_url"`
	PriceSyncIntervalMinutes int     `yaml:"price_sync_interval_minutes"`
	FastPricingMode          string  `yaml:"fast_pricing_mode"`
	FastMultiplier           float64 `yaml:"fast_multiplier"`
	PricingMode              string  `yaml:"pricing_mode"`
	ApplyFastPricing         bool    `yaml:"apply_fast_pricing"`
	LongContextThreshold     int64   `yaml:"long_context_threshold"`
	ApplyLongContextPricing  bool    `yaml:"apply_long_context_pricing"`
	HistoryDays              int     `yaml:"history_days"`
}

func defaultConfig() config {
	return config{
		ApplyModelCalibration:    true,
		AstraMultiplier:          1.8,
		Enabled:                  true,
		DataPath:                 "/CLIProxyAPI/data/cpa-quota-estimator.sqlite",
		SampleIntervalMinutes:    5,
		PriceSourceURL:           "https://models.dev/catalog.json",
		PriceSyncIntervalMinutes: 1440,
		FastPricingMode:          "multiplier",
		FastMultiplier:           2.5,
		PricingMode:              pricingModeLegacyAPI,
		ApplyFastPricing:         true,
		LongContextThreshold:     272000,
		ApplyLongContextPricing:  false,
		HistoryDays:              365,
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
	RequestedAt            int64
	ObservedAt             int64
	Account                string
	Provider               string
	Model                  string
	Alias                  string
	ServiceTier            string
	InputTokens            int64
	OutputTokens           int64
	ReasoningTokens        int64
	CacheReadTokens        int64
	CacheWriteTokens       int64
	TotalTokens            int64
	CostUSD                float64
	Failed                 bool
	StatusCode             int
	UsedPercent            *float64
	ResetAt                int64
	WindowMinutes          int64
	SecondaryUsedPercent   *float64
	SecondaryResetAt       int64
	SecondaryWindowMinutes int64
	PlanType               string
	QuotaScope             string
}

type quotaPoint struct {
	CycleID       int64   `json:"cycle_id"`
	CycleStart    int64   `json:"cycle_start"`
	Time          int64   `json:"time"`
	UsedPercent   float64 `json:"used_percent"`
	ResetAt       int64   `json:"reset_at"`
	WindowMinutes int64   `json:"window_minutes"`
	WindowTokens  int64   `json:"window_tokens"`
	WindowCostUSD float64 `json:"window_cost_usd"`
	Requests      int64   `json:"requests"`
}

type scopedQuotaPoint struct {
	Time          int64   `json:"time"`
	UsedPercent   float64 `json:"used_percent"`
	ResetAt       int64   `json:"reset_at"`
	WindowMinutes int64   `json:"window_minutes"`
	PlanType      string  `json:"plan_type"`
	WindowTokens  int64   `json:"window_tokens"`
	WindowCostUSD float64 `json:"window_cost_usd"`
	Requests      int64   `json:"requests"`
}

type scopedQuotaSeries struct {
	Scope            string             `json:"scope"`
	StartedAt        int64              `json:"started_at"`
	ResetAt          int64              `json:"reset_at"`
	WindowMinutes    int64              `json:"window_minutes"`
	PlanType         string             `json:"plan_type"`
	UsedPercent      float64            `json:"used_percent"`
	ObservationCount int64              `json:"observation_count"`
	LastObservedAt   int64              `json:"last_observed_at,omitempty"`
	ScheduleInferred bool               `json:"schedule_inferred,omitempty"`
	Points           []scopedQuotaPoint `json:"points"`
	CapacityPoints   []capacityPoint    `json:"capacity_points"`
	Estimate         estimate           `json:"estimate"`
	RemainingByModel []modelAllowance   `json:"remaining_by_model,omitempty"`
}

type quotaCycle struct {
	ID               int64   `json:"id"`
	StartedAt        int64   `json:"started_at"`
	EndedAt          int64   `json:"ended_at,omitempty"`
	ResetAt          int64   `json:"reset_at"`
	WindowMinutes    int64   `json:"window_minutes"`
	PlanType         string  `json:"plan_type"`
	CloseReason      string  `json:"close_reason,omitempty"`
	FirstSampleAt    int64   `json:"first_sample_at"`
	LastSampleAt     int64   `json:"last_sample_at"`
	StartPercent     float64 `json:"start_percent"`
	EndPercent       float64 `json:"end_percent"`
	PeakPercent      float64 `json:"peak_percent"`
	ActualTokens     int64   `json:"actual_tokens"`
	ActualCostUSD    float64 `json:"actual_cost_usd"`
	Requests         int64   `json:"requests"`
	Current          bool    `json:"current"`
	ObservedComplete bool    `json:"observed_complete"`
	ScheduleInferred bool    `json:"schedule_inferred,omitempty"`
}

type quotaWindow struct {
	ResetAt       int64   `json:"reset_at"`
	WindowStart   int64   `json:"window_start"`
	WindowMinutes int64   `json:"window_minutes"`
	PlanType      string  `json:"plan_type"`
	FirstSampleAt int64   `json:"first_sample_at"`
	LastSampleAt  int64   `json:"last_sample_at"`
	StartPercent  float64 `json:"start_percent"`
	EndPercent    float64 `json:"end_percent"`
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

type modelAllowance struct {
	ModelMultiplier float64 `json:"model_multiplier"`
	Model           string  `json:"model"`
	PricingMode     string  `json:"pricing_mode"`
	ValueUnit       string  `json:"value_unit"`
	InputRate       float64 `json:"input_rate"`
	OutputRate      float64 `json:"output_rate"`
	CacheReadRate   float64 `json:"cache_read_rate"`
	InputTokens     float64 `json:"input_tokens"`
	OutputTokens    float64 `json:"output_tokens"`
	CacheReadTokens float64 `json:"cache_read_tokens"`
}

type capacityPoint struct {
	CycleID           int64   `json:"cycle_id"`
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

type accountQuotaOverview struct {
	Scope            string            `json:"scope"`
	WindowStart      int64             `json:"window_start,omitempty"`
	ResetAt          int64             `json:"reset_at,omitempty"`
	WindowMinutes    int64             `json:"window_minutes,omitempty"`
	ScheduleInferred bool              `json:"schedule_inferred,omitempty"`
	RemainingPercent float64           `json:"remaining_percent"`
	QuotaStatus      string            `json:"quota_status"`
	Latest           *scopedQuotaPoint `json:"latest,omitempty"`
	Estimate         estimate          `json:"estimate"`
	BurnForecast     burnForecast      `json:"burn_forecast"`
}

type accountOverview struct {
	Account               string                `json:"account"`
	PlanType              string                `json:"plan_type"`
	SelectedCycleID       int64                 `json:"selected_cycle_id,omitempty"`
	WindowStart           int64                 `json:"window_start,omitempty"`
	ResetAt               int64                 `json:"reset_at,omitempty"`
	WindowMinutes         int64                 `json:"window_minutes,omitempty"`
	IsCurrent             bool                  `json:"is_current"`
	ScheduleInferred      bool                  `json:"schedule_inferred,omitempty"`
	RemainingPercent      float64               `json:"remaining_percent"`
	QuotaStatus           string                `json:"quota_status"`
	Latest                *quotaPoint           `json:"latest,omitempty"`
	Estimate              estimate              `json:"estimate"`
	BurnForecast          burnForecast          `json:"burn_forecast"`
	FiveHourQuotaDetected bool                  `json:"five_hour_quota_detected"`
	WeeklyQuota           *accountQuotaOverview `json:"weekly_quota,omitempty"`
}

type overviewResponse struct {
	PluginVersion string            `json:"plugin_version"`
	PricingMode   string            `json:"pricing_mode"`
	ValueUnit     string            `json:"value_unit"`
	Accounts      []accountOverview `json:"accounts"`
}

type monthlyCycle struct {
	quotaCycle
	MonthTokens       int64   `json:"month_tokens"`
	MonthCostUSD      float64 `json:"month_cost_usd"`
	MonthRequests     int64   `json:"month_requests"`
	CapacityAvailable bool    `json:"capacity_available"`
	FullWindowTokens  float64 `json:"full_window_tokens"`
	FullWindowCostUSD float64 `json:"full_window_cost_usd"`
	TokenLow          float64 `json:"token_low"`
	TokenHigh         float64 `json:"token_high"`
	CostLow           float64 `json:"cost_low"`
	CostHigh          float64 `json:"cost_high"`
	Confidence        string  `json:"confidence"`
}

type monthlySummary struct {
	Month                   string         `json:"month"`
	Timezone                string         `json:"timezone"`
	StartAt                 int64          `json:"start_at"`
	EndAt                   int64          `json:"end_at"`
	ActualTokens            int64          `json:"actual_tokens"`
	ActualCostUSD           float64        `json:"actual_cost_usd"`
	Requests                int64          `json:"requests"`
	CycleCount              int            `json:"cycle_count"`
	ResetCount              int            `json:"reset_count"`
	EarlyResetCount         int            `json:"early_reset_count"`
	AllocatedCycleCount     int            `json:"allocated_cycle_count"`
	EstimatedCycleCount     int            `json:"estimated_cycle_count"`
	ConsumedQuotaPercent    float64        `json:"consumed_quota_percent"`
	ConsumedQuotaEquivalent float64        `json:"consumed_quota_equivalent"`
	QuotaCoverageComplete   bool           `json:"quota_coverage_complete"`
	UnusedQuotaAtReset      float64        `json:"unused_quota_at_reset"`
	EstimatedTokens         float64        `json:"estimated_tokens"`
	EstimatedTokenLow       float64        `json:"estimated_token_low"`
	EstimatedTokenHigh      float64        `json:"estimated_token_high"`
	EstimatedCostUSD        float64        `json:"estimated_cost_usd"`
	EstimatedCostLow        float64        `json:"estimated_cost_low"`
	EstimatedCostHigh       float64        `json:"estimated_cost_high"`
	Cycles                  []monthlyCycle `json:"cycles"`
}
