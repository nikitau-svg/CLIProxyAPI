package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

var currentConfig atomic.Value

const (
	defaultQuotaUsageRefreshSeconds       = 15 * 60
	minimumQuotaUsageRefreshSeconds       = 5 * 60
	maximumQuotaUsageRefreshSeconds       = 24 * 60 * 60
	defaultAdaptiveCoolingHalfLifeSeconds = 5 * 60
	defaultAdaptiveCoolingMaxAgeSeconds   = 30 * 60
	minimumAdaptiveCoolingHalfLifeSeconds = 60
	maximumAdaptiveCoolingMaxAgeSeconds   = 24 * 60 * 60
)

// selfOnlyDefaultModels are connected Codex models with no counterpart on the
// Claude side. They get a logical alias with a single self-candidate: routing
// them to a mapped equivalent would silently answer from a different model,
// while leaving them unregistered let the host serve them outside Bravo
// entirely. See the comment at their registration in defaultPluginConfig.
var selfOnlyDefaultModels = []string{"codex-auto-review", "gpt-5.3-codex-spark"}

func defaultPluginConfig() pluginConfig {
	caps := []string{"text", "tools", "tool_result", "vision", "web_search", "stream"}
	// Both providers declare reasoning. Claude replays a signed thinking block
	// verbatim; Codex carries the reasoning text across as prior context and
	// cannot resume the signed state — see the liveCapabilityMatrix comment in
	// contract.go. Withholding it from Codex made a request with replayed thinking
	// unroutable the moment every Claude credential ran out of weekly quota, which
	// is a worse outcome than answering with the reasoning as context.
	textCaps := append(append([]string(nil), caps...), capabilityReasoning)
	profiles := compatibilityProfiles()
	withReviewedStructuredOutput := func(provider, model string, capabilities []string) []string {
		out := append([]string(nil), capabilities...)
		profile, ok := profiles[routeCandidateKey(provider, model)]
		if !ok {
			return out
		}
		if _, verified := newCapabilitySet(profile.Capabilities...)[capabilityStructuredOutput]; verified {
			out = append(out, capabilityStructuredOutput)
		}
		return out
	}
	imageCaps := []string{capabilityImageGeneration}
	claude := func(model, effort string, priority int) candidate {
		return candidate{Provider: "claude", Model: model, Effort: effort, Priority: priority, Capabilities: withReviewedStructuredOutput("claude", model, textCaps)}
	}
	codex := func(model, effort string, priority int) candidate {
		return candidate{Provider: "codex", Model: model, Effort: effort, Priority: priority, Capabilities: withReviewedStructuredOutput("codex", model, textCaps)}
	}
	codexImage := func(model string, priority int) candidate {
		return candidate{Provider: "codex", Model: model, Priority: priority, Capabilities: append([]string(nil), imageCaps...)}
	}
	models := map[string]logicalModel{
		"frontier": {
			DisplayName: "Bravo Frontier",
			Description: "Maximum-quality pool across the newest Claude and OpenAI subscriptions.",
			Candidates: []candidate{
				claude("claude-fable-5", "max", 100),
				codex("gpt-5.6-sol", "max", 90),
			},
		},
		"deep": {
			DisplayName: "Bravo Deep",
			Description: "Deliberate reasoning pool, preferring Opus and Sol.",
			Candidates: []candidate{
				claude("claude-opus-5", "high", 100),
				codex("gpt-5.6-sol", "high", 90),
			},
		},
		"balanced": {
			DisplayName: "Bravo Balanced",
			Description: "Balanced quality and latency across Sonnet and Terra.",
			Candidates: []candidate{
				claude("claude-sonnet-5", "medium", 100),
				codex("gpt-5.6-terra", "medium", 90),
			},
		},
		"fast": {
			DisplayName: "Bravo Fast",
			Description: "Low-latency pool across Luna and the equivalent Haiku tier.",
			Candidates: []candidate{
				codex("gpt-5.6-luna", "low", 100),
				claude("claude-haiku-4-5-20251001", "low", 90),
			},
		},
		"opus": {
			DisplayName: "Bravo Opus",
			Description: "Opus subscriptions first, then the equivalent Sol pool.",
			Candidates: []candidate{
				claude("claude-opus-5", "high", 100),
				codex("gpt-5.6-sol", "high", 90),
			},
		},
		"fable": {
			DisplayName: "Bravo Fable",
			Description: "Fable at maximum effort with the equivalent Sol pool.",
			Candidates: []candidate{
				claude("claude-fable-5", "max", 100),
				codex("gpt-5.6-sol", "max", 90),
			},
		},
		"fabulus": {
			DisplayName: "Bravo Fabulus",
			Description: "Friendly alias for the Fable/Sol maximum-effort class.",
			Candidates: []candidate{
				claude("claude-fable-5", "max", 100),
				codex("gpt-5.6-sol", "max", 90),
			},
		},
		"sonnet": {
			DisplayName: "Bravo Sonnet",
			Description: "Sonnet subscriptions first, then the equivalent Terra pool.",
			Candidates: []candidate{
				claude("claude-sonnet-5", "medium", 100),
				codex("gpt-5.6-terra", "medium", 90),
			},
		},
		"haiku": {
			DisplayName: "Bravo Haiku",
			Description: "Haiku subscriptions first, then the equivalent Luna pool.",
			Candidates: []candidate{
				claude("claude-haiku-4-5-20251001", "low", 100),
				codex("gpt-5.6-luna", "low", 90),
			},
		},
		"sol": {
			DisplayName: "Bravo Sol",
			Description: "Sol subscriptions first, then the equivalent Opus pool.",
			Candidates: []candidate{
				codex("gpt-5.6-sol", "max", 100),
				claude("claude-opus-5", "max", 90),
			},
		},
		"terra": {
			DisplayName: "Bravo Terra",
			Description: "Terra subscriptions first, then the equivalent Sonnet pool.",
			Candidates: []candidate{
				codex("gpt-5.6-terra", "medium", 100),
				claude("claude-sonnet-5", "medium", 90),
			},
		},
		"luna": {
			DisplayName: "Bravo Luna",
			Description: "Luna first, then the equivalent Haiku pool.",
			Candidates: []candidate{
				codex("gpt-5.6-luna", "low", 100),
				claude("claude-haiku-4-5-20251001", "low", 90),
			},
		},
		"image": {
			DisplayName: "Bravo Image",
			Description: "Smart image-generation pool across connected OpenAI image subscriptions.",
			Candidates: []candidate{
				codexImage("gpt-image-2", 100),
				codexImage("gpt-image-1.5", 90),
			},
		},
		"gpt-image-2": {
			DisplayName: "Bravo GPT Image 2",
			Description: "GPT Image 2 subscriptions first, then GPT Image 1.5 without a text-model fallback.",
			Candidates: []candidate{
				codexImage("gpt-image-2", 100),
				codexImage("gpt-image-1.5", 90),
			},
		},
		"gpt-image-1.5": {
			DisplayName: "Bravo GPT Image 1.5",
			Description: "GPT Image 1.5 subscriptions first, then GPT Image 2 without a text-model fallback.",
			Candidates: []candidate{
				codexImage("gpt-image-1.5", 100),
				codexImage("gpt-image-2", 90),
			},
		},
	}

	// Every general text model exposed by the connected Claude and Codex
	// subscriptions gets an exact logical alias, so a client may address a real
	// model name and still be routed, metered and limited by Bravo.
	//
	// codex-auto-review and gpt-5.3-codex-spark are included with a single
	// self-candidate and no cross-provider equivalent. They have no Claude
	// counterpart, so a mapped fallback would silently answer from a different
	// model. Leaving them out entirely was worse: routeModel declined them, the
	// host served them natively, and the request escaped project scope and quota
	// accounting altogether — verified against production, where a smart key
	// asking for codex-auto-review was answered without Bravo recording an event.
	exact := map[string][]candidate{
		"claude-fable-5":             {claude("claude-fable-5", "max", 100), codex("gpt-5.6-sol", "max", 90)},
		"claude-opus-5":              {claude("claude-opus-5", "high", 100), codex("gpt-5.6-sol", "high", 90)},
		"claude-sonnet-5":            {claude("claude-sonnet-5", "medium", 100), codex("gpt-5.6-terra", "medium", 90)},
		"claude-opus-4-8":            {claude("claude-opus-4-8", "high", 100), codex("gpt-5.6-sol", "high", 90)},
		"claude-opus-4-7":            {claude("claude-opus-4-7", "high", 100), codex("gpt-5.6-sol", "high", 90)},
		"claude-opus-4-6":            {claude("claude-opus-4-6", "high", 100), codex("gpt-5.6-sol", "high", 90)},
		"claude-sonnet-4-6":          {claude("claude-sonnet-4-6", "medium", 100), codex("gpt-5.6-terra", "medium", 90)},
		"claude-opus-4-5-20251101":   {claude("claude-opus-4-5-20251101", "high", 100), codex("gpt-5.5", "high", 90)},
		"claude-sonnet-4-5-20250929": {claude("claude-sonnet-4-5-20250929", "medium", 100), codex("gpt-5.6-terra", "medium", 90)},
		"claude-haiku-4-5-20251001":  {claude("claude-haiku-4-5-20251001", "low", 100), codex("gpt-5.6-luna", "low", 90)},
		"claude-opus-4-1-20250805":   {claude("claude-opus-4-1-20250805", "high", 100), codex("gpt-5.5", "high", 90)},
		"claude-opus-4-20250514":     {claude("claude-opus-4-20250514", "high", 100), codex("gpt-5.5", "high", 90)},
		"claude-sonnet-4-20250514":   {claude("claude-sonnet-4-20250514", "medium", 100), codex("gpt-5.4", "medium", 90)},
		"claude-3-7-sonnet-20250219": {claude("claude-3-7-sonnet-20250219", "medium", 100), codex("gpt-5.4", "medium", 90)},
		"claude-3-5-haiku-20241022":  {claude("claude-3-5-haiku-20241022", "", 100), codex("gpt-5.4-mini", "low", 90)},
		"gpt-5.6-sol":                {codex("gpt-5.6-sol", "max", 100), claude("claude-fable-5", "max", 90)},
		"gpt-5.6-terra":              {codex("gpt-5.6-terra", "medium", 100), claude("claude-sonnet-5", "medium", 90)},
		"gpt-5.6-luna":               {codex("gpt-5.6-luna", "low", 100), claude("claude-haiku-4-5-20251001", "low", 90)},
		"gpt-5.5":                    {codex("gpt-5.5", "high", 100), claude("claude-opus-4-6", "high", 90)},
		"gpt-5.4":                    {codex("gpt-5.4", "medium", 100), claude("claude-sonnet-4-6", "medium", 90)},
		"gpt-5.4-mini":               {codex("gpt-5.4-mini", "low", 100), claude("claude-haiku-4-5-20251001", "low", 90)},
	}
	for name, candidates := range exact {
		models[name] = logicalModel{
			DisplayName: "Bravo " + name,
			Description: "Exact " + name + " subscriptions first, then mapped equivalents.",
			Candidates:  candidates,
		}
	}
	for _, name := range selfOnlyDefaultModels {
		models[name] = logicalModel{
			DisplayName: "Bravo " + name,
			Description: "Exact " + name + " subscriptions only; this model has no equivalent to fall back to.",
			Candidates:  []candidate{codex(name, "", 100)},
		}
	}
	// "auto" intentionally points at the explicit frontier policy so clients
	// can ask for a stable generic model name.
	models["auto"] = models["frontier"]

	return pluginConfig{
		Enabled:                           true,
		Prefix:                            defaultPrefix,
		RequireSmartKey:                   false,
		MaxAttempts:                       0,
		CooldownSeconds:                   30,
		CompactBypassCooldownSeconds:      15 * 60,
		FallbackHedgeDelaySeconds:         40,
		StatePath:                         defaultStatePath,
		AllocatorMode:                     "enforce",
		AdaptiveAllocatorMode:             "observe",
		AdaptiveCoolingHalfLifeSeconds:    defaultAdaptiveCoolingHalfLifeSeconds,
		AdaptiveCoolingMaxAgeSeconds:      defaultAdaptiveCoolingMaxAgeSeconds,
		QuotaRefreshSeconds:               defaultQuotaUsageRefreshSeconds,
		QuotaUsageRefreshSeconds:          defaultQuotaUsageRefreshSeconds,
		QuotaUsageMaxStaleSeconds:         60 * 60,
		QuotaProfileRefreshSeconds:        6 * 60 * 60,
		QuotaRefreshJitterPercent:         20,
		QuotaRefreshProviderMinIntervalMS: 500,
		QuotaRefreshProviderConcurrency:   1,
		UnknownSecondaryPolicy:            "block",
		Tariffs: []tariffConfig{
			{ID: "x1", SessionFloorPercent: 50, WeeklyFloorPercent: 50, Multiplier: 1, ReservationPercent: 0.5},
			{ID: "x5", SessionFloorPercent: 30, WeeklyFloorPercent: 30, Multiplier: 5, ReservationPercent: 0.1},
			{ID: "x20", SessionFloorPercent: 20, WeeklyFloorPercent: 20, Multiplier: 20, ReservationPercent: 0.05},
		},
		Models:             models,
		PersistedTariffIDs: make(map[string]bool),
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return fmt.Errorf("decode Bravo config: %w", errUnmarshal)
		}
		var presence struct {
			Tariffs                  *[]tariffConfig `yaml:"tariffs"`
			QuotaUsageRefreshSeconds *int            `yaml:"quota_usage_refresh_seconds"`
		}
		if errPresence := yaml.Unmarshal(req.ConfigYAML, &presence); errPresence != nil {
			return fmt.Errorf("decode Bravo tariff presence: %w", errPresence)
		}
		cfg.PersistedTariffIDs = make(map[string]bool)
		if presence.QuotaUsageRefreshSeconds == nil {
			cfg.QuotaUsageRefreshSeconds = cfg.QuotaRefreshSeconds
		}
		// 0.8.0 persisted the historical one-minute cache TTL in some installs.
		// Treat every formerly accepted sub-five-minute value as an upgrade
		// marker instead of failing plugin startup or preserving a provider-hostile
		// polling rate forever.
		if cfg.QuotaUsageRefreshSeconds > 0 && cfg.QuotaUsageRefreshSeconds < minimumQuotaUsageRefreshSeconds {
			cfg.QuotaUsageRefreshSeconds = defaultQuotaUsageRefreshSeconds
		}
		if presence.Tariffs != nil {
			for _, tariff := range *presence.Tariffs {
				id := strings.ToLower(strings.TrimSpace(tariff.ID))
				if id != "" {
					cfg.PersistedTariffIDs[id] = true
				}
			}
		}
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		return errNormalize
	}
	if errState := configureUsageState(cfg.StatePath); errState != nil {
		return fmt.Errorf("configure Bravo state: %w", errState)
	}
	if errTraces := configureRouteTraceStore(cfg.StatePath); errTraces != nil {
		return fmt.Errorf("configure Bravo route traces: %w", errTraces)
	}
	configureAdaptiveShadowAuditStore(cfg.StatePath)
	currentConfig.Store(cfg)
	quotaPollingConfigured.Store(true)
	wakeQuotaPolling()
	return nil
}

func normalizeConfig(cfg *pluginConfig) error {
	if cfg == nil {
		return fmt.Errorf("Bravo config is nil")
	}
	cfg.Prefix = strings.TrimSpace(cfg.Prefix)
	if cfg.Prefix == "" {
		cfg.Prefix = defaultPrefix
	}
	if !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	if cfg.MaxAttempts < 0 {
		return fmt.Errorf("max_attempts must be zero or positive")
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = 30
	}
	if cfg.CompactBypassCooldownSeconds < 0 {
		return fmt.Errorf("compact_bypass_cooldown_seconds must be zero or positive")
	}
	if cfg.FallbackHedgeDelaySeconds < 0 {
		return fmt.Errorf("fallback_hedge_delay_seconds must be zero or positive")
	}
	cfg.StatePath = strings.TrimSpace(cfg.StatePath)
	if cfg.StatePath == "" {
		cfg.StatePath = defaultStatePath
	}
	cfg.AllocatorMode = strings.ToLower(strings.TrimSpace(cfg.AllocatorMode))
	switch cfg.AllocatorMode {
	case "":
		cfg.AllocatorMode = "enforce"
	case "off", "observe", "enforce":
	default:
		return fmt.Errorf("allocator_mode must be off, observe, or enforce")
	}
	cfg.AdaptiveAllocatorMode = strings.ToLower(strings.TrimSpace(cfg.AdaptiveAllocatorMode))
	switch cfg.AdaptiveAllocatorMode {
	case "":
		cfg.AdaptiveAllocatorMode = "observe"
	case "off", "observe", "breaker", "assist", "enforce":
	default:
		return fmt.Errorf("adaptive_allocator_mode must be off, observe, breaker, assist, or enforce")
	}
	if cfg.AdaptiveCoolingHalfLifeSeconds <= 0 {
		cfg.AdaptiveCoolingHalfLifeSeconds = defaultAdaptiveCoolingHalfLifeSeconds
	}
	if cfg.AdaptiveCoolingHalfLifeSeconds < minimumAdaptiveCoolingHalfLifeSeconds ||
		cfg.AdaptiveCoolingHalfLifeSeconds > maximumAdaptiveCoolingMaxAgeSeconds {
		return fmt.Errorf("adaptive_cooling_half_life_seconds must be between %d and %d", minimumAdaptiveCoolingHalfLifeSeconds, maximumAdaptiveCoolingMaxAgeSeconds)
	}
	if cfg.AdaptiveCoolingMaxAgeSeconds <= 0 {
		cfg.AdaptiveCoolingMaxAgeSeconds = defaultAdaptiveCoolingMaxAgeSeconds
	}
	if cfg.AdaptiveCoolingMaxAgeSeconds < cfg.AdaptiveCoolingHalfLifeSeconds ||
		cfg.AdaptiveCoolingMaxAgeSeconds > maximumAdaptiveCoolingMaxAgeSeconds {
		return fmt.Errorf("adaptive_cooling_max_age_seconds must be between adaptive_cooling_half_life_seconds and %d", maximumAdaptiveCoolingMaxAgeSeconds)
	}
	if cfg.QuotaRefreshSeconds <= 0 {
		cfg.QuotaRefreshSeconds = defaultQuotaUsageRefreshSeconds
	}
	if cfg.QuotaUsageRefreshSeconds <= 0 {
		cfg.QuotaUsageRefreshSeconds = defaultQuotaUsageRefreshSeconds
	}
	if cfg.QuotaUsageRefreshSeconds < minimumQuotaUsageRefreshSeconds || cfg.QuotaUsageRefreshSeconds > maximumQuotaUsageRefreshSeconds {
		return fmt.Errorf("quota_usage_refresh_seconds must be between %d and %d", minimumQuotaUsageRefreshSeconds, maximumQuotaUsageRefreshSeconds)
	}
	if cfg.QuotaUsageMaxStaleSeconds < cfg.QuotaUsageRefreshSeconds {
		cfg.QuotaUsageMaxStaleSeconds = max(cfg.QuotaUsageRefreshSeconds, 15*60)
	}
	if cfg.QuotaProfileRefreshSeconds <= 0 {
		cfg.QuotaProfileRefreshSeconds = 6 * 60 * 60
	}
	if cfg.QuotaRefreshJitterPercent < 0 || cfg.QuotaRefreshJitterPercent > 100 {
		return fmt.Errorf("quota_refresh_jitter_percent must be between 0 and 100")
	}
	if cfg.QuotaRefreshProviderMinIntervalMS < 0 {
		return fmt.Errorf("quota_refresh_provider_min_interval_ms must be zero or positive")
	}
	if cfg.QuotaRefreshProviderConcurrency <= 0 {
		cfg.QuotaRefreshProviderConcurrency = 1
	}
	cfg.UnknownSecondaryPolicy = strings.ToLower(strings.TrimSpace(cfg.UnknownSecondaryPolicy))
	switch cfg.UnknownSecondaryPolicy {
	case "":
		cfg.UnknownSecondaryPolicy = "block"
	case "block", "allow":
	default:
		return fmt.Errorf("unknown_secondary_policy must be block or allow")
	}
	if cfg.PersistedTariffIDs == nil {
		cfg.PersistedTariffIDs = make(map[string]bool)
	}
	presentTariffs := make(map[string]struct{}, len(cfg.Tariffs))
	for _, tariff := range cfg.Tariffs {
		presentTariffs[strings.ToLower(strings.TrimSpace(tariff.ID))] = struct{}{}
	}
	for _, builtIn := range defaultPluginConfig().Tariffs {
		if _, exists := presentTariffs[builtIn.ID]; exists {
			continue
		}
		cfg.Tariffs = append(cfg.Tariffs, builtIn)
		presentTariffs[builtIn.ID] = struct{}{}
	}
	seenTariffs := make(map[string]struct{}, len(cfg.Tariffs))
	for index := range cfg.Tariffs {
		tariff := &cfg.Tariffs[index]
		tariff.ID = strings.ToLower(strings.TrimSpace(tariff.ID))
		if tariff.ID == "" {
			return fmt.Errorf("tariffs[%d] requires id", index)
		}
		if _, exists := seenTariffs[tariff.ID]; exists {
			return fmt.Errorf("duplicate tariff id %q", tariff.ID)
		}
		seenTariffs[tariff.ID] = struct{}{}
		if tariff.SessionFloorPercent < 0 || tariff.SessionFloorPercent >= 100 {
			return fmt.Errorf("tariffs[%d] session_floor_percent must be in [0,100)", index)
		}
		if tariff.WeeklyFloorPercent < 0 || tariff.WeeklyFloorPercent >= 100 {
			return fmt.Errorf("tariffs[%d] weekly_floor_percent must be in [0,100)", index)
		}
		if tariff.Multiplier <= 0 {
			return fmt.Errorf("tariffs[%d] multiplier must be positive", index)
		}
		if tariff.ReservationPercent <= 0 || tariff.ReservationPercent >= 100 {
			return fmt.Errorf("tariffs[%d] reservation_percent must be in (0,100)", index)
		}
	}
	if _, exists := seenTariffs["x1"]; !exists {
		return fmt.Errorf("tariffs must include x1")
	}
	seenSubscriptions := make(map[string]struct{}, len(cfg.Subscriptions))
	for index := range cfg.Subscriptions {
		subscription := &cfg.Subscriptions[index]
		subscription.AuthIndex = strings.TrimSpace(subscription.AuthIndex)
		subscription.Tariff = strings.ToLower(strings.TrimSpace(subscription.Tariff))
		if subscription.AuthIndex == "" {
			return fmt.Errorf("subscriptions[%d] requires auth_index", index)
		}
		if _, exists := seenSubscriptions[subscription.AuthIndex]; exists {
			return fmt.Errorf("duplicate subscription auth_index %q", subscription.AuthIndex)
		}
		seenSubscriptions[subscription.AuthIndex] = struct{}{}
		if subscription.Tariff == "" {
			subscription.Tariff = "auto"
		}
		if subscription.Tariff != "auto" {
			if _, exists := seenTariffs[subscription.Tariff]; !exists {
				return fmt.Errorf("subscriptions[%d] references unknown tariff %q", index, subscription.Tariff)
			}
		}
	}
	baseModels := cfg.BaseModels
	if len(baseModels) == 0 {
		baseModels = cfg.Models
	}
	if len(baseModels) == 0 {
		return fmt.Errorf("at least one Bravo logical model is required")
	}
	normalizedModels, errModels := normalizeLogicalModels(cfg.Prefix, baseModels)
	if errModels != nil {
		return errModels
	}
	cfg.BaseModels = cloneLogicalModels(normalizedModels)
	cfg.Models = cloneLogicalModels(normalizedModels)
	if errOverrides := normalizeAndApplyRouteOverrides(cfg); errOverrides != nil {
		return errOverrides
	}

	seenIDs := make(map[string]struct{}, len(cfg.SmartKeys))
	seenNames := make([]string, 0, len(cfg.SmartKeys))
	seenDigests := make(map[string]struct{}, len(cfg.SmartKeys))
	for index := range cfg.SmartKeys {
		key := &cfg.SmartKeys[index]
		key.ID = strings.TrimSpace(key.ID)
		normalizedName, nameFailure := normalizeProjectName(key.Name)
		if nameFailure != nil {
			return fmt.Errorf("smart_keys[%d] has invalid name: %s", index, nameFailure.Message)
		}
		key.Name = normalizedName
		key.SHA256 = strings.ToLower(strings.TrimSpace(key.SHA256))
		key.Models = normalizeStrings(key.Models)
		key.PrimaryAuthIDs = normalizeOpaqueStrings(key.PrimaryAuthIDs)
		key.AllowedAuthIDs = normalizeOpaqueStrings(key.AllowedAuthIDs)
		key.Status = strings.ToLower(strings.TrimSpace(key.Status))
		promptCache, promptCacheFailure := normalizeProjectPromptCachePolicy(key.Policy)
		if promptCacheFailure != nil {
			return fmt.Errorf("smart_keys[%d] has invalid prompt cache policy: %s", index, promptCacheFailure.Message)
		}
		if key.Policy != nil {
			if _, exists := key.Policy["prompt_cache"]; exists {
				key.Policy["prompt_cache"] = map[string]any{
					"anthropic_ttl": promptCache.AnthropicTTL,
				}
			}
		}
		if key.Name == "" || len(key.SHA256) != 64 {
			return fmt.Errorf("smart_keys[%d] requires name and a 64-character sha256", index)
		}
		digest, errDigest := hex.DecodeString(key.SHA256)
		if errDigest != nil || len(digest) != 32 {
			return fmt.Errorf("smart_keys[%d] sha256 must be lowercase hexadecimal", index)
		}
		if key.ID == "" {
			key.ID = legacyProjectID(*key)
			key.LegacyDerivedID = true
		}
		if !validProjectID(key.ID) {
			return fmt.Errorf("smart_keys[%d] has invalid id", index)
		}
		enabled := smartKeyEnabled(*key)
		if key.Status == "" {
			if enabled {
				key.Status = "active"
			} else {
				key.Status = "disabled"
			}
		}
		switch key.Status {
		case "active":
			if !enabled {
				return fmt.Errorf("smart_keys[%d] active status conflicts with enabled=false", index)
			}
		case "disabled", "revoked":
			if enabled {
				return fmt.Errorf("smart_keys[%d] %s status conflicts with enabled=true", index, key.Status)
			}
		default:
			return fmt.Errorf("smart_keys[%d] has unsupported status %q", index, key.Status)
		}
		if _, exists := seenIDs[key.ID]; exists {
			return fmt.Errorf("duplicate smart key id %q", key.ID)
		}
		seenIDs[key.ID] = struct{}{}
		for _, seenName := range seenNames {
			if strings.EqualFold(seenName, key.Name) {
				return fmt.Errorf("duplicate smart key name %q", key.Name)
			}
		}
		seenNames = append(seenNames, key.Name)
		if _, exists := seenDigests[key.SHA256]; exists {
			return fmt.Errorf("duplicate smart key digest")
		}
		seenDigests[key.SHA256] = struct{}{}
		for _, model := range key.Models {
			if model == "*" {
				continue
			}
			if _, exists := cfg.Models[strings.Trim(model, "/")]; !exists {
				return fmt.Errorf("smart_keys[%d] references unknown model %q", index, model)
			}
		}
		if len(key.AllowedAuthIDs) > 0 {
			allowed := make(map[string]struct{}, len(key.AllowedAuthIDs))
			for _, authID := range key.AllowedAuthIDs {
				allowed[authID] = struct{}{}
			}
			for _, primaryID := range key.PrimaryAuthIDs {
				if _, exists := allowed[primaryID]; !exists {
					return fmt.Errorf("smart_keys[%d] primary auth %q is outside allowed_auth_ids", index, primaryID)
				}
			}
		}
	}
	return nil
}

func normalizeLogicalModels(prefix string, models map[string]logicalModel) (map[string]logicalModel, error) {
	normalizedModels := make(map[string]logicalModel, len(models))
	for rawName, model := range models {
		name := strings.ToLower(strings.Trim(strings.TrimSpace(rawName), "/"))
		if name == "" || strings.ContainsAny(name, " \t\r\n()") {
			return nil, fmt.Errorf("invalid logical model name %q", rawName)
		}
		if len(model.Candidates) == 0 {
			return nil, fmt.Errorf("logical model %s has no candidates", name)
		}
		for index := range model.Candidates {
			item := &model.Candidates[index]
			item.Provider = strings.ToLower(strings.TrimSpace(item.Provider))
			item.Model = strings.TrimSpace(item.Model)
			item.Effort = normalizeEffort(item.Effort)
			if item.Provider == "" || item.Model == "" {
				return nil, fmt.Errorf("logical model %s candidate %d requires provider and model", name, index)
			}
			if !supportedEffort(item.Effort) {
				return nil, fmt.Errorf("logical model %s candidate %d has unsupported effort %q", name, index, item.Effort)
			}
			item.Capabilities = normalizeStrings(item.Capabilities)
			if len(item.Capabilities) == 0 {
				item.Capabilities = []string{"text"}
			}
			item.AuthIDs = normalizeStrings(item.AuthIDs)
		}
		sort.SliceStable(model.Candidates, func(i, j int) bool {
			return model.Candidates[i].Priority > model.Candidates[j].Priority
		})
		if strings.TrimSpace(model.DisplayName) == "" {
			model.DisplayName = prefix + name
		}
		normalizedModels[name] = model
	}
	return normalizedModels, nil
}

func smartKeyEnabled(key smartKeyConfig) bool {
	if key.Enabled != nil {
		return *key.Enabled
	}
	switch strings.ToLower(strings.TrimSpace(key.Status)) {
	case "disabled", "revoked":
		return false
	default:
		return true
	}
}

func smartKeyActive(key smartKeyConfig) bool {
	if !smartKeyEnabled(key) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(key.Status))
	return status == "" || status == "active"
}

func legacyProjectID(key smartKeyConfig) string {
	digest := strings.ToLower(strings.TrimSpace(key.SHA256))
	if len(digest) >= 16 {
		return "legacy_" + digest[:16]
	}
	return ""
}

func validProjectID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	cfg := defaultPluginConfig()
	_ = normalizeConfig(&cfg)
	return cfg
}

// immutableRoutingConfigSnapshot detaches every persisted slice, map, and
// project policy from the atomically loaded config. A later management reload
// may be built from a shallow copy, so retaining its backing arrays across an
// async stream ACK would otherwise mutate an already accepted request.
func immutableRoutingConfigSnapshot(cfg pluginConfig) pluginConfig {
	snapshot := cfg
	snapshot.Tariffs = append([]tariffConfig(nil), cfg.Tariffs...)
	snapshot.Subscriptions = append([]subscriptionConfig(nil), cfg.Subscriptions...)
	for index := range snapshot.Subscriptions {
		if cfg.Subscriptions[index].Enabled != nil {
			enabled := *cfg.Subscriptions[index].Enabled
			snapshot.Subscriptions[index].Enabled = &enabled
		}
	}
	snapshot.SmartKeys = make([]smartKeyConfig, len(cfg.SmartKeys))
	for index, project := range cfg.SmartKeys {
		snapshot.SmartKeys[index] = cloneSmartKeyConfig(project)
	}
	snapshot.RouteOverrides = make([]routeOverrideConfig, len(cfg.RouteOverrides))
	for index, override := range cfg.RouteOverrides {
		snapshot.RouteOverrides[index] = override
		snapshot.RouteOverrides[index].Candidates = cloneRoutingCandidates(override.Candidates)
	}
	snapshot.Models = cloneRoutingModels(cfg.Models)
	snapshot.BaseModels = cloneRoutingModels(cfg.BaseModels)
	snapshot.PersistedTariffIDs = make(map[string]bool, len(cfg.PersistedTariffIDs))
	for id, persisted := range cfg.PersistedTariffIDs {
		snapshot.PersistedTariffIDs[id] = persisted
	}
	return snapshot
}

func cloneRoutingModels(models map[string]logicalModel) map[string]logicalModel {
	if models == nil {
		return nil
	}
	cloned := make(map[string]logicalModel, len(models))
	for id, model := range models {
		model.Candidates = cloneRoutingCandidates(model.Candidates)
		cloned[id] = model
	}
	return cloned
}

func cloneRoutingCandidates(candidates []candidate) []candidate {
	cloned := append([]candidate(nil), candidates...)
	for index := range cloned {
		cloned[index].Capabilities = append([]string(nil), candidates[index].Capabilities...)
		cloned[index].AuthIDs = append([]string(nil), candidates[index].AuthIDs...)
	}
	return cloned
}

func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return ""
	case "ultra":
		return "max"
	case "minimal", "low", "medium", "high", "xhigh", "max", "auto", "none":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func supportedEffort(value string) bool {
	switch value {
	case "", "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func normalizeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeOpaqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
