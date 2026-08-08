package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginIdentifier = "bravo"
	pluginVersion    = "0.8.5"
	defaultPrefix    = "bravo/"
	// Keep Bravo's own state outside CLIProxyAPI's auth directory. Files placed
	// in /root/.cli-proxy-api are discovered as credentials by the host.
	defaultStatePath = "bravo-data/bravo-state.json"

	projectPromptCacheTTLAutomatic  = "auto"
	projectPromptCacheTTL5Minutes   = "5m"
	projectPromptCacheTTL1Hour      = "1h"
	projectPromptCacheOpenAIManaged = "provider_managed"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code          string                `json:"code"`
	Message       string                `json:"message"`
	Retryable     bool                  `json:"retryable,omitempty"`
	HTTPStatus    int                   `json:"http_status,omitempty"`
	Headers       http.Header           `json:"headers,omitempty"`
	RetryAfter    string                `json:"retry_after,omitempty"`
	ProviderError *providererror.Detail `json:"provider_error,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	FrontendAuthProvider  bool     `json:"frontend_auth_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	Scheduler             bool     `json:"scheduler"`
	UsagePlugin           bool     `json:"usage_plugin"`
	ManagementAPI         bool     `json:"management_api"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type pluginConfig struct {
	Enabled                           bool                    `yaml:"enabled" json:"enabled"`
	Prefix                            string                  `yaml:"prefix" json:"prefix"`
	RequireSmartKey                   bool                    `yaml:"require_smart_key" json:"require_smart_key"`
	MaxAttempts                       int                     `yaml:"max_attempts" json:"max_attempts"`
	CooldownSeconds                   int                     `yaml:"cooldown_seconds" json:"cooldown_seconds"`
	CompactBypassCooldownSeconds      int                     `yaml:"compact_bypass_cooldown_seconds" json:"compact_bypass_cooldown_seconds"`
	FallbackHedgeDelaySeconds         int                     `yaml:"fallback_hedge_delay_seconds" json:"fallback_hedge_delay_seconds"`
	StatePath                         string                  `yaml:"state_path" json:"state_path"`
	AllocatorMode                     string                  `yaml:"allocator_mode" json:"allocator_mode"`
	QuotaRefreshSeconds               int                     `yaml:"quota_refresh_seconds" json:"quota_refresh_seconds"`
	QuotaUsageRefreshSeconds          int                     `yaml:"quota_usage_refresh_seconds" json:"quota_usage_refresh_seconds"`
	QuotaUsageMaxStaleSeconds         int                     `yaml:"quota_usage_max_stale_seconds" json:"quota_usage_max_stale_seconds"`
	QuotaProfileRefreshSeconds        int                     `yaml:"quota_profile_refresh_seconds" json:"quota_profile_refresh_seconds"`
	QuotaRefreshJitterPercent         int                     `yaml:"quota_refresh_jitter_percent" json:"quota_refresh_jitter_percent"`
	QuotaRefreshProviderMinIntervalMS int                     `yaml:"quota_refresh_provider_min_interval_ms" json:"quota_refresh_provider_min_interval_ms"`
	QuotaRefreshProviderConcurrency   int                     `yaml:"quota_refresh_provider_concurrency" json:"quota_refresh_provider_concurrency"`
	UnknownSecondaryPolicy            string                  `yaml:"unknown_secondary_policy" json:"unknown_secondary_policy"`
	Tariffs                           []tariffConfig          `yaml:"tariffs" json:"tariffs"`
	Subscriptions                     []subscriptionConfig    `yaml:"subscriptions" json:"subscriptions"`
	SmartKeys                         []smartKeyConfig        `yaml:"smart_keys" json:"smart_keys"`
	RouteOverrides                    []routeOverrideConfig   `yaml:"route_overrides" json:"route_overrides"`
	Models                            map[string]logicalModel `yaml:"models" json:"models"`
	// BaseModels is the normalized pre-override model map. It is runtime-only so
	// deleting an override can restore the configured/default route exactly.
	BaseModels map[string]logicalModel `yaml:"-" json:"-"`
	// PersistedTariffIDs distinguishes injected built-in defaults from actual
	// list items, so the first UI edit appends instead of replacing a missing
	// host config entry.
	PersistedTariffIDs map[string]bool `yaml:"-" json:"-"`
}

type tariffConfig struct {
	ID                  string  `yaml:"id" json:"id"`
	SessionFloorPercent float64 `yaml:"session_floor_percent" json:"session_floor_percent"`
	WeeklyFloorPercent  float64 `yaml:"weekly_floor_percent" json:"weekly_floor_percent"`
	Multiplier          float64 `yaml:"multiplier" json:"multiplier"`
	ReservationPercent  float64 `yaml:"reservation_percent" json:"reservation_percent"`
}

type subscriptionConfig struct {
	AuthIndex string `yaml:"auth_index" json:"auth_index"`
	Tariff    string `yaml:"tariff" json:"tariff"`
	Enabled   *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

type smartKeyConfig struct {
	ID              string         `yaml:"id" json:"id"`
	Name            string         `yaml:"name" json:"name"`
	SHA256          string         `yaml:"sha256" json:"sha256"`
	Enabled         *bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Status          string         `yaml:"status,omitempty" json:"status,omitempty"`
	Models          []string       `yaml:"models" json:"models"`
	PrimaryAuthIDs  []string       `yaml:"primary_auth_ids,omitempty" json:"primary_auth_ids,omitempty"`
	AllowedAuthIDs  []string       `yaml:"allowed_auth_ids,omitempty" json:"allowed_auth_ids,omitempty"`
	Policy          map[string]any `yaml:"policy,omitempty" json:"policy,omitempty"`
	CreatedAt       string         `yaml:"created_at,omitempty" json:"created_at,omitempty"`
	UpdatedAt       string         `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
	LegacyDerivedID bool           `yaml:"-" json:"-"`
}

// projectPromptCachePolicy is persisted inside smart_keys[].policy.prompt_cache
// so older Bravo configs remain compatible while the management API can expose
// a small, validated provider-native contract.
type projectPromptCachePolicy struct {
	AnthropicTTL string `json:"anthropic_ttl" yaml:"anthropic_ttl"`
}

type projectPromptCacheView struct {
	AnthropicTTL string `json:"anthropic_ttl"`
	OpenAIMode   string `json:"openai_mode"`
}

type logicalModel struct {
	DisplayName string      `yaml:"display_name" json:"display_name"`
	Description string      `yaml:"description" json:"description"`
	Candidates  []candidate `yaml:"candidates" json:"candidates"`
}

type routeOverrideConfig struct {
	ID         string      `yaml:"id" json:"id"`
	Candidates []candidate `yaml:"candidates" json:"candidates"`
}

type candidate struct {
	Provider     string   `yaml:"provider" json:"provider"`
	Model        string   `yaml:"model" json:"model"`
	Effort       string   `yaml:"effort" json:"effort"`
	Priority     int      `yaml:"priority" json:"priority"`
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	AuthIDs      []string `yaml:"auth_ids" json:"auth_ids"`
}

type executionAttempt struct {
	LogicalModel                 string
	Candidate                    candidate
	Auth                         pluginapi.HostAuthFileEntry
	RequestedEffort              string
	EffectiveEffort              string
	ProjectID                    string
	Primary                      bool
	AllocatorManaged             bool
	ReservationPercent           float64
	TariffID                     string
	CompactBypass                bool
	CompactBypassKey             string
	CompactBypassCooldownSeconds int
	PreflightRejections          []candidateRejection
}

type attemptRecord struct {
	At                       time.Time `json:"at"`
	LogicalModel             string    `json:"logical_model"`
	Provider                 string    `json:"provider"`
	Model                    string    `json:"model"`
	Effort                   string    `json:"effort,omitempty"`
	RequestedEffort          string    `json:"requested_effort,omitempty"`
	EffectiveEffort          string    `json:"effective_effort,omitempty"`
	AuthID                   string    `json:"auth_id"`
	AuthLabel                string    `json:"auth_label,omitempty"`
	Status                   int       `json:"status"`
	Success                  bool      `json:"success"`
	Retryable                bool      `json:"retryable,omitempty"`
	ErrorCode                string    `json:"error_code,omitempty"`
	Error                    string    `json:"error,omitempty"`
	ProviderErrorType        string    `json:"provider_error_type,omitempty"`
	ProviderErrorCode        string    `json:"provider_error_code,omitempty"`
	ProviderModel            string    `json:"provider_model,omitempty"`
	ProviderModelDisplayName string    `json:"provider_model_display_name,omitempty"`
	ProviderNoticeTitle      string    `json:"provider_notice_title,omitempty"`
	ProviderNoticeText       string    `json:"provider_notice_text,omitempty"`
	ProviderDisabledReason   string    `json:"provider_disabled_reason,omitempty"`
	ProviderErrorReason      string    `json:"provider_error_reason,omitempty"`
	Scope                    string    `json:"scope,omitempty"`
	CompactBypass            bool      `json:"compact_bypass,omitempty"`
	LatencyMS                int64     `json:"latency_ms"`
}

type hostCallError struct {
	Code          string
	Message       string
	Retryable     bool
	HTTPStatus    int
	Headers       http.Header
	RetryAfter    string
	ProviderError *providererror.Detail
}

func (e *hostCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

func (e *hostCallError) ProviderErrorDetail() (providererror.Detail, bool) {
	if e == nil || e.ProviderError == nil {
		return providererror.Detail{}, false
	}
	detail := providererror.Sanitize(*e.ProviderError)
	if detail.Code == "" && detail.Type == "" && detail.Message == "" {
		return providererror.Detail{}, false
	}
	return detail, true
}

func sanitizedProviderErrorPointer(source *providererror.Detail) *providererror.Detail {
	if source == nil {
		return nil
	}
	detail := providererror.Sanitize(*source)
	if detail.Code == "" && detail.Type == "" && detail.Message == "" {
		return nil
	}
	return &detail
}

type capabilitySet map[string]struct{}
