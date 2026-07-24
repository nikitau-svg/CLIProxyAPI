package main

import "sort"

// compatibilityProfile is a reviewed Bravo-side declaration. Host discovery
// alone never creates one: a new physical model remains fail-closed until its
// provider contract and capabilities have been verified.
type compatibilityProfile struct {
	Provider            string
	Model               string
	DefaultEffort       string
	Capabilities        []string
	RecommendedRoutes   []string
	HasThinkingPolicy   bool
	ThinkingDefaultOn   bool
	ThinkingZeroAllowed bool
	MaxDisableLevel     string
}

func compatibilityProfiles() map[string]compatibilityProfile {
	textCaps := []string{
		capabilityText,
		capabilityTools,
		capabilityToolResult,
		capabilityVision,
		capabilityWebSearch,
		capabilityStream,
	}
	claudeCaps := append(append([]string(nil), textCaps...), capabilityReasoning)
	imageCaps := []string{capabilityImageGeneration}
	profiles := []compatibilityProfile{
		{Provider: "claude", Model: "claude-fable-5", DefaultEffort: "max", Capabilities: claudeCaps, RecommendedRoutes: []string{"frontier"}, HasThinkingPolicy: true, ThinkingDefaultOn: true},
		{Provider: "claude", Model: "claude-opus-5", DefaultEffort: "high", Capabilities: claudeCaps, RecommendedRoutes: []string{"opus"}, HasThinkingPolicy: true, ThinkingDefaultOn: true, ThinkingZeroAllowed: true, MaxDisableLevel: "high"},
		{Provider: "claude", Model: "claude-sonnet-5", DefaultEffort: "medium", Capabilities: claudeCaps, RecommendedRoutes: []string{"sonnet"}, HasThinkingPolicy: true, ThinkingDefaultOn: true, ThinkingZeroAllowed: true},
		{Provider: "claude", Model: "claude-opus-4-8", DefaultEffort: "high", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-opus-4-7", DefaultEffort: "high", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-opus-4-6", DefaultEffort: "high", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-sonnet-4-6", DefaultEffort: "medium", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-opus-4-5-20251101", DefaultEffort: "high", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-sonnet-4-5-20250929", DefaultEffort: "medium", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-haiku-4-5-20251001", DefaultEffort: "low", Capabilities: claudeCaps, RecommendedRoutes: []string{"haiku"}},
		{Provider: "claude", Model: "claude-opus-4-1-20250805", DefaultEffort: "high", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-opus-4-20250514", DefaultEffort: "high", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-sonnet-4-20250514", DefaultEffort: "medium", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-3-7-sonnet-20250219", DefaultEffort: "medium", Capabilities: claudeCaps},
		{Provider: "claude", Model: "claude-3-5-haiku-20241022", Capabilities: claudeCaps},
		{Provider: "codex", Model: "gpt-5.6-sol", DefaultEffort: "max", Capabilities: textCaps, RecommendedRoutes: []string{"sol"}},
		{Provider: "codex", Model: "gpt-5.6-terra", DefaultEffort: "medium", Capabilities: textCaps, RecommendedRoutes: []string{"terra"}},
		{Provider: "codex", Model: "gpt-5.6-luna", DefaultEffort: "low", Capabilities: textCaps, RecommendedRoutes: []string{"luna"}},
		{Provider: "codex", Model: "gpt-5.5", DefaultEffort: "high", Capabilities: textCaps},
		{Provider: "codex", Model: "gpt-5.4", DefaultEffort: "medium", Capabilities: textCaps},
		{Provider: "codex", Model: "gpt-5.4-mini", DefaultEffort: "low", Capabilities: textCaps},
		{Provider: "codex", Model: "gpt-image-2", Capabilities: imageCaps, RecommendedRoutes: []string{"image"}},
		{Provider: "codex", Model: "gpt-image-1.5", Capabilities: imageCaps, RecommendedRoutes: []string{"image"}},
	}

	result := make(map[string]compatibilityProfile, len(profiles))
	for _, profile := range profiles {
		profile.Provider = normalizeProvider(profile.Provider)
		profile.Model = normalizeCompatibilityModel(profile.Model)
		profile.DefaultEffort = normalizeEffort(profile.DefaultEffort)
		profile.MaxDisableLevel = normalizeEffort(profile.MaxDisableLevel)
		profile.Capabilities = normalizeStrings(profile.Capabilities)
		profile.RecommendedRoutes = normalizeStrings(profile.RecommendedRoutes)
		sort.Strings(profile.RecommendedRoutes)
		result[routeCandidateKey(profile.Provider, profile.Model)] = profile
	}
	return result
}

func compatibilityProviderInScope(provider string) bool {
	switch normalizeProvider(provider) {
	case "claude", "codex":
		return true
	default:
		return false
	}
}

func compatibilityModelExcluded(provider, model string) bool {
	if normalizeProvider(provider) != "codex" {
		return false
	}
	switch normalizeCompatibilityModel(model) {
	case "codex-auto-review", "gpt-5.3-codex-spark":
		return true
	default:
		return false
	}
}

func normalizeCompatibilityModel(model string) string {
	return normalizeRouteID(model)
}
