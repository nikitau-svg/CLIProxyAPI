package main

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// A provider may expose 128k+ output models. Keep the bounded scanner, but
	// never truncate a valid declaration to the former 64k ceiling and thereby
	// price it like a cheaper request. One million tokens already reaches the
	// reservation context cap and is a conservative bound for unknown futures.
	adaptiveMaximumOutputTokenBudget = 1024 * 1024.0
	adaptiveContextCapBodyBytes      = 2 * 1024 * 1024
)

// The tariff reservation is the operator's minimum accounting unit. The
// adaptive layer only raises it: an uncertain estimate must never weaken an
// explicitly configured floor.
const (
	adaptiveMaximumReservationPercent = 10.0
	adaptiveMaximumLearnedScale       = 8.0
	adaptiveCommitHorizon             = 2 * time.Minute
	adaptiveWindowSession             = "session"
	adaptiveWindowWeekly              = "weekly"
	adaptiveMaximumExposure           = 60 * time.Minute
	adaptiveMaximumRecentCommitments  = 256
	adaptiveProfileStateTTL           = 24 * time.Hour
	adaptiveMaximumProfileEntries     = 4096
	adaptiveMaximumOverflowEntries    = 4096
)

type adaptiveReserveCommit struct {
	At      time.Time
	Percent float64
}

type adaptiveReserveProfile struct {
	AuthIndex         string
	Shape             adaptiveRequestShape
	Session           adaptiveWindowEstimate
	Weekly            adaptiveWindowEstimate
	UnobservedPercent float64
	// Aggregate fields remain a compatibility view for persisted v0 profiles.
	LearnedScale       float64
	ObservedBurnPerMin float64
	RecentCommitments  []adaptiveReserveCommit
	UpdatedAt          time.Time
}

type adaptiveWindowEstimate struct {
	LearnedScale       float64
	ObservedBurnPerMin float64
	StableIntervals    int
}

type adaptiveRefreshWatermark struct {
	Buckets               map[string]float64
	Total                 float64
	PendingPercent        float64
	OrphanPreparedPercent float64
	ObservePendingPercent float64
	CapturedAt            time.Time
}

var adaptiveReserveRuntime = struct {
	sync.Mutex
	Profiles         map[string]*adaptiveReserveProfile
	Buckets          map[string]*adaptiveReserveProfile
	Overflow         map[string]*adaptiveReserveProfile
	Saturated        map[string]time.Time
	SaturationGlobal bool
}{
	Profiles:  make(map[string]*adaptiveReserveProfile),
	Buckets:   make(map[string]*adaptiveReserveProfile),
	Overflow:  make(map[string]*adaptiveReserveProfile),
	Saturated: make(map[string]time.Time),
}

type adaptiveRequestShape struct {
	Multiplier    float64
	ModelFamily   string
	Provider      string
	PhysicalModel string
	EffortBucket  string
	ContextBucket string
	CostMode      string
}

type adaptiveRequestFeatures struct {
	Tokens            float64
	ContextMultiplier float64
	ContextBucket     string
	CostMode          string
}

// buildAdaptiveRequestShape is a narrow test seam used to prove that a large
// request is decoded once per candidate, not once per eligible credential.
var buildAdaptiveRequestShape = adaptiveRequestShapeFor
var buildAdaptiveRequestFeatures = adaptiveRequestFeaturesFor

// adaptiveReservationPercent prices the uncertainty of accepting one request.
// It does not call a provider. That keeps the ordinary hot path deterministic
// while confirmed quota polling remains owned by the background worker.
func adaptiveReservationPercent(
	req rpcExecutorRequest,
	auth pluginapi.HostAuthFileEntry,
	item candidate,
	tariff tariffConfig,
	now time.Time,
) float64 {
	shape := buildAdaptiveRequestShape(executionBodyView(req), item)
	return adaptiveReservationForShape(auth, tariff, shape, now)
}

func adaptiveRequestShapeFor(body []byte, item candidate) adaptiveRequestShape {
	return adaptiveRequestShapeFromFeatures(buildAdaptiveRequestFeatures(body), item)
}

func adaptiveRequestFeaturesFor(body []byte) adaptiveRequestFeatures {
	tokens := estimatedRequestTokens(body)
	return adaptiveRequestFeatures{
		Tokens: tokens, ContextMultiplier: adaptiveContextFactor(tokens), ContextBucket: adaptiveContextBucket(tokens),
		CostMode: adaptiveRequestCostMode(body),
	}
}

func adaptiveRequestShapeFromFeatures(features adaptiveRequestFeatures, item candidate) adaptiveRequestShape {
	return adaptiveRequestShape{Multiplier: adaptiveModelFactor(item.Model) *
		adaptiveEffortFactor(item.Effort) *
		features.ContextMultiplier,
		ModelFamily: adaptiveModelFamily(item.Model), Provider: normalizeProvider(item.Provider),
		PhysicalModel: strings.ToLower(strings.TrimSpace(item.Model)),
		EffortBucket:  adaptiveEffortBucket(item.Effort), ContextBucket: features.ContextBucket,
		CostMode: features.CostMode,
	}
}

func adaptiveRequestCostMode(body []byte) string {
	// Cost-affecting flags are normally top-level. Bound inspection so a 4MB
	// prompt stays O(1); anything missing, escaped, or beyond the trusted prefix
	// is placed in an explicit conservative unknown bucket rather than sharing
	// a cheap cached/tool-free training identity.
	const trustedPrefix = 256 * 1024
	view := body
	truncated := false
	if len(view) > trustedPrefix {
		view, truncated = view[:trustedPrefix], true
	}
	parts := make([]string, 0, 6)
	for _, field := range []struct {
		key, label string
	}{
		{`"cache_control"`, "cache"}, {`"tools"`, "tools"}, {`"reasoning"`, "reasoning"},
		{`"background"`, "background"}, {`"stream"`, "stream"},
	} {
		if bytes.Contains(view, []byte(field.key)) {
			parts = append(parts, field.label)
		}
	}
	if truncated || bytes.Contains(view, []byte(`\u`)) || len(parts) == 0 {
		parts = append(parts, "unknown")
	}
	sort.Strings(parts)
	return strings.Join(parts, "+")
}

func adaptiveReservationForShape(
	auth pluginapi.HostAuthFileEntry,
	tariff tariffConfig,
	shape adaptiveRequestShape,
	now time.Time,
) float64 {
	baseline := tariff.ReservationPercent
	if baseline <= 0 {
		baseline = 0.1
	}
	authIndex := strings.TrimSpace(auth.AuthIndex)
	adaptiveReserveRuntime.Lock()
	_, identitySaturated := adaptiveReserveRuntime.Saturated[authIndex]
	identitySaturated = identitySaturated || adaptiveReserveRuntime.SaturationGlobal
	adaptiveReserveRuntime.Unlock()
	if identitySaturated {
		// Secondary borrowing is blocked by the exposure guard. Primary keeps
		// its floor=0 semantics, but must not recreate an evicted estimator as a
		// cheap learned=1 request; reserve the conservative maximum instead.
		return clampAdaptiveReservation(adaptiveMaximumReservationPercent, baseline)
	}
	learned, recentRate := adaptiveProfileFactorsForShape(authIndex, shape, now)
	return adaptiveReservationFromFactors(tariff, shape, baseline, learned, recentRate)
}

// adaptiveReservationForShapePeek is the read-only management/trace path. It
// never creates a bucket, updates UpdatedAt, prunes commitments, or changes
// saturation state merely because an operator opened the dashboard.
func adaptiveReservationForShapePeek(
	auth pluginapi.HostAuthFileEntry,
	tariff tariffConfig,
	shape adaptiveRequestShape,
	now time.Time,
) float64 {
	baseline := tariff.ReservationPercent
	if baseline <= 0 {
		baseline = 0.1
	}
	authIndex := strings.TrimSpace(auth.AuthIndex)
	key := adaptiveProfileKey(authIndex, shape)
	learned, recentRate := 1.0, 0.0
	adaptiveReserveRuntime.Lock()
	_, saturated := adaptiveReserveRuntime.Saturated[authIndex]
	saturated = saturated || adaptiveReserveRuntime.SaturationGlobal
	if profile := adaptiveReserveRuntime.Buckets[key]; profile != nil {
		learned = math.Max(profile.Session.LearnedScale, profile.Weekly.LearnedScale)
		cutoff := now.UTC().Add(-adaptiveCommitHorizon)
		for _, item := range profile.RecentCommitments {
			if !item.At.Before(cutoff) {
				recentRate += item.Percent
			}
		}
		recentRate /= adaptiveCommitHorizon.Minutes()
	}
	if learned <= 1 {
		if aggregate := adaptiveReserveRuntime.Profiles[authIndex]; aggregate != nil {
			learned = math.Max(learned, aggregate.LearnedScale)
		}
		if overflow := adaptiveReserveRuntime.Overflow[authIndex]; overflow != nil {
			learned = math.Max(learned, overflow.LearnedScale)
		}
	}
	adaptiveReserveRuntime.Unlock()
	if saturated {
		return clampAdaptiveReservation(adaptiveMaximumReservationPercent, baseline)
	}
	return adaptiveReservationFromFactors(tariff, shape, baseline, learned, recentRate)
}

func adaptiveReservationFromFactors(tariff tariffConfig, shape adaptiveRequestShape, baseline, learned, recentRate float64) float64 {

	// A rapid burst deserves a larger next reservation even before the next
	// quota snapshot. PendingPercent still accounts for every accepted request;
	// this multiplier limits how many equal-cost requests can enter together.
	burstScale := 1.0
	if recentRate > baseline {
		burstScale += math.Min(recentRate/math.Max(baseline*20, 0.1), 2)
	}
	// Provider observations and floors are already percentages of this exact
	// subscription's capacity. Applying the tariff multiplier to learned scale
	// again would make an observed 8x x20 underprediction look like only 1.35x.
	// Learned actual/predicted scale is therefore unattenuated. Multiplier only
	// normalizes an additional cold-start shape prior:
	//   reservation = baseline*shape*learned*burst
	//               + baseline*(shape-1)/max(tariffMultiplier, 1)
	// This keeps the incident-safe base prior, makes x5/x20 no larger than x1,
	// and still lets provider evidence converge at its true percentage ratio.
	shapeScale := math.Max(shape.Multiplier, 1)
	value := baseline*shapeScale*math.Max(learned, 1)*burstScale +
		baseline*(shapeScale-1)/math.Max(tariff.Multiplier, 1)
	return clampAdaptiveReservation(value, baseline)
}

func adaptiveModelFamily(model string) string {
	value := strings.ToLower(strings.TrimSpace(model))
	for _, family := range []string{"fable", "opus", "sonnet", "haiku", "codex", "gpt"} {
		if strings.Contains(value, family) {
			return family
		}
	}
	return firstNonEmpty(value, "unknown")
}

func adaptiveEffortBucket(effort string) string {
	value := normalizeEffort(effort)
	if value == "" || value == "none" || value == "auto" {
		return "standard"
	}
	return value
}

func adaptiveContextBucket(tokens float64) string {
	switch {
	case tokens <= 16*1024:
		return "small"
	case tokens <= 64*1024:
		return "medium"
	case tokens <= 256*1024:
		return "large"
	default:
		return "xlarge"
	}
}

func adaptiveProfileKey(authIndex string, shape adaptiveRequestShape) string {
	return strings.Join([]string{
		strings.TrimSpace(authIndex), shape.Provider, shape.PhysicalModel, shape.ModelFamily,
		shape.EffortBucket, shape.ContextBucket, shape.CostMode,
	}, "\x1f")
}

func adaptiveModelFactor(model string) float64 {
	value := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(value, "fable"):
		return 2.2
	case strings.Contains(value, "opus"):
		return 2.0
	case strings.Contains(value, "sonnet"):
		return 1.25
	case strings.Contains(value, "haiku"):
		return 0.7
	case strings.Contains(value, "codex"), strings.Contains(value, "gpt"):
		return 1.35
	default:
		return 1
	}
}

func adaptiveEffortFactor(effort string) float64 {
	switch normalizeEffort(effort) {
	case "minimal", "low":
		return 0.8
	case "medium", "auto", "none", "":
		return 1
	case "high":
		return 1.25
	case "xhigh":
		return 1.6
	case "ultra":
		return 2
	default:
		return 1.15
	}
}

// estimatedRequestTokens intentionally stays local and conservative. Exact
// tokenization would couple the allocator to every provider vocabulary and add
// latency; byte size plus the declared output budget is stable across both the
// OpenAI and Anthropic request shapes.
func estimatedRequestTokens(body []byte) float64 {
	estimate := float64(len(body)) / 4
	if len(body) >= adaptiveContextCapBodyBytes {
		return math.Max(estimate, 1024)
	}
	budget, trusted := declaredOutputTokenBudgetValue(body)
	if !trusted || budget <= 0 {
		budget = adaptiveMaximumOutputTokenBudget
	}
	estimate += budget
	return math.Max(estimate, 1024)
}

func declaredOutputTokenBudget(body []byte) float64 {
	budget, _ := declaredOutputTokenBudgetValue(body)
	return budget
}

func declaredOutputTokenBudgetValue(body []byte) (float64, bool) {
	cursor := skipAdaptiveJSONWhitespace(body, 0)
	if cursor >= len(body) || body[cursor] != '{' {
		return adaptiveMaximumOutputTokenBudget, false
	}
	cursor++
	maximum, found := uint64(0), false
	for {
		cursor = skipAdaptiveJSONWhitespace(body, cursor)
		if cursor >= len(body) {
			return adaptiveMaximumOutputTokenBudget, false
		}
		if body[cursor] == '}' {
			cursor = skipAdaptiveJSONWhitespace(body, cursor+1)
			if cursor != len(body) {
				return adaptiveMaximumOutputTokenBudget, false
			}
			return float64(maximum), found
		}
		if body[cursor] != '"' {
			return adaptiveMaximumOutputTokenBudget, false
		}
		keyStart := cursor + 1
		keyEnd, escaped, ok := scanAdaptiveJSONString(body, cursor)
		if !ok {
			return adaptiveMaximumOutputTokenBudget, false
		}
		// Decoding escaped top-level keys would add work and ambiguity to the hot
		// path. Treat them as unknown so an escaped max_tokens can never masquerade
		// as a cheap request.
		if escaped {
			return adaptiveMaximumOutputTokenBudget, false
		}
		key := body[keyStart : keyEnd-1]
		cursor = skipAdaptiveJSONWhitespace(body, keyEnd)
		if cursor >= len(body) || body[cursor] != ':' {
			return adaptiveMaximumOutputTokenBudget, false
		}
		cursor = skipAdaptiveJSONWhitespace(body, cursor+1)
		if adaptiveOutputTokenKey(key) {
			value, next, valid := scanAdaptiveJSONUnsignedInteger(body, cursor)
			if !valid || value > uint64(adaptiveMaximumOutputTokenBudget) {
				return adaptiveMaximumOutputTokenBudget, false
			}
			if value > maximum {
				maximum = value
			}
			found = true
			cursor = next
		} else {
			var valid bool
			cursor, valid = skipAdaptiveJSONValue(body, cursor)
			if !valid {
				return adaptiveMaximumOutputTokenBudget, false
			}
		}
		cursor = skipAdaptiveJSONWhitespace(body, cursor)
		if cursor >= len(body) {
			return adaptiveMaximumOutputTokenBudget, false
		}
		switch body[cursor] {
		case ',':
			cursor++
		case '}':
			continue
		default:
			return adaptiveMaximumOutputTokenBudget, false
		}
	}
}

func adaptiveOutputTokenKey(key []byte) bool {
	return bytes.Equal(key, []byte("max_tokens")) ||
		bytes.Equal(key, []byte("max_output_tokens")) ||
		bytes.Equal(key, []byte("max_completion_tokens"))
}

func skipAdaptiveJSONWhitespace(body []byte, cursor int) int {
	for cursor < len(body) {
		switch body[cursor] {
		case ' ', '\t', '\r', '\n':
			cursor++
		default:
			return cursor
		}
	}
	return cursor
}

// scanAdaptiveJSONString returns the byte immediately after the closing quote.
// It validates control bytes and escape shape without allocating or decoding.
func scanAdaptiveJSONString(body []byte, cursor int) (int, bool, bool) {
	if cursor >= len(body) || body[cursor] != '"' {
		return cursor, false, false
	}
	escaped := false
	for cursor++; cursor < len(body); cursor++ {
		switch body[cursor] {
		case '"':
			return cursor + 1, escaped, true
		case '\\':
			escaped = true
			cursor++
			if cursor >= len(body) {
				return cursor, escaped, false
			}
			if body[cursor] == 'u' {
				if cursor+4 >= len(body) {
					return cursor, escaped, false
				}
				for index := cursor + 1; index <= cursor+4; index++ {
					if !adaptiveJSONHex(body[index]) {
						return cursor, escaped, false
					}
				}
				cursor += 4
			} else {
				switch body[cursor] {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				default:
					return cursor, escaped, false
				}
			}
		default:
			if body[cursor] < 0x20 {
				return cursor, escaped, false
			}
		}
	}
	return cursor, escaped, false
}

func adaptiveJSONHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func scanAdaptiveJSONUnsignedInteger(body []byte, cursor int) (uint64, int, bool) {
	if cursor >= len(body) || body[cursor] < '0' || body[cursor] > '9' {
		return 0, cursor, false
	}
	start := cursor
	value := uint64(0)
	for cursor < len(body) && body[cursor] >= '0' && body[cursor] <= '9' {
		digit := uint64(body[cursor] - '0')
		if value > (uint64(adaptiveMaximumOutputTokenBudget)-digit)/10 {
			return uint64(adaptiveMaximumOutputTokenBudget) + 1, cursor, false
		}
		value = value*10 + digit
		cursor++
	}
	if cursor-start > 1 && body[start] == '0' {
		return 0, cursor, false
	}
	next := skipAdaptiveJSONWhitespace(body, cursor)
	if next >= len(body) || body[next] != ',' && body[next] != '}' {
		return 0, cursor, false
	}
	return value, cursor, true
}

func skipAdaptiveJSONValue(body []byte, cursor int) (int, bool) {
	if cursor >= len(body) {
		return cursor, false
	}
	if body[cursor] == '"' {
		next, _, ok := scanAdaptiveJSONString(body, cursor)
		return next, ok
	}
	if body[cursor] != '{' && body[cursor] != '[' {
		start := cursor
		for cursor < len(body) && body[cursor] != ',' && body[cursor] != '}' && body[cursor] != ']' &&
			body[cursor] != ' ' && body[cursor] != '\t' && body[cursor] != '\r' && body[cursor] != '\n' {
			cursor++
		}
		return cursor, cursor > start
	}
	var stack [128]byte
	depth := 1
	stack[0] = body[cursor]
	for cursor++; cursor < len(body); cursor++ {
		switch body[cursor] {
		case '"':
			next, _, ok := scanAdaptiveJSONString(body, cursor)
			if !ok {
				return cursor, false
			}
			cursor = next - 1
		case '{', '[':
			if depth >= len(stack) {
				return cursor, false
			}
			stack[depth] = body[cursor]
			depth++
		case '}', ']':
			want := byte('{')
			if body[cursor] == ']' {
				want = '['
			}
			if depth == 0 || stack[depth-1] != want {
				return cursor, false
			}
			depth--
			if depth == 0 {
				return cursor + 1, true
			}
		}
	}
	return cursor, false
}

func adaptiveContextFactor(tokens float64) float64 {
	// 8k is the neutral coding turn. sqrt keeps a 1M context expensive without
	// allowing one declared maximum to monopolise an entire subscription.
	return math.Min(math.Max(math.Sqrt(math.Max(tokens, 8192)/8192), 1), 8)
}

func clampAdaptiveReservation(value, baseline float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return baseline
	}
	maximum := math.Max(adaptiveMaximumReservationPercent, baseline)
	return math.Min(math.Max(value, baseline), maximum)
}

func adaptiveProfileFactors(authIndex string, now time.Time) (learnedScale, recentPercentPerMinute float64) {
	if authIndex == "" {
		return 1, 0
	}
	adaptiveReserveRuntime.Lock()
	defer adaptiveReserveRuntime.Unlock()
	profile := adaptiveReserveRuntime.Profiles[authIndex]
	if profile == nil {
		return 1, 0
	}
	pruneAdaptiveCommitments(profile, now)
	for _, item := range profile.RecentCommitments {
		recentPercentPerMinute += item.Percent
	}
	recentPercentPerMinute /= adaptiveCommitHorizon.Minutes()
	return math.Max(profile.LearnedScale, 1), recentPercentPerMinute
}

func adaptiveProfileFactorsForShape(authIndex string, shape adaptiveRequestShape, now time.Time) (learnedScale, recentPercentPerMinute float64) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return 1, 0
	}
	key := adaptiveProfileKey(authIndex, shape)
	adaptiveReserveRuntime.Lock()
	profile := ensureAdaptiveBucketLocked(key, authIndex, shape)
	profile.UpdatedAt = now.UTC()
	pruneAdaptiveCommitments(profile, now)
	for _, item := range profile.RecentCommitments {
		recentPercentPerMinute += item.Percent
	}
	recentPercentPerMinute /= adaptiveCommitHorizon.Minutes()
	learning := math.Max(profile.Session.LearnedScale, profile.Weekly.LearnedScale)
	if learning <= 1 {
		if aggregate := adaptiveReserveRuntime.Profiles[authIndex]; aggregate != nil {
			learning = math.Max(learning, aggregate.LearnedScale)
		}
		if overflow := adaptiveReserveRuntime.Overflow[authIndex]; overflow != nil {
			learning = math.Max(learning, overflow.LearnedScale)
		}
	}
	adaptiveReserveRuntime.Unlock()
	return math.Max(learning, 1), recentPercentPerMinute
}

func recordAdaptiveReservationCommit(authIndex string, percent float64, at time.Time) {
	recordAdaptiveReservationCommitForKey(authIndex, authIndex, percent, at)
}

func recordAdaptiveReservationCommitForKey(authIndex, profileKey string, percent float64, at time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || percent <= 0 {
		return
	}
	adaptiveReserveRuntime.Lock()
	profile := adaptiveReserveRuntime.Buckets[profileKey]
	if profile == nil {
		profile = ensureAdaptiveBucketLocked(profileKey, authIndex, adaptiveShapeFromProfileKey(profileKey))
	}
	profile.RecentCommitments = append(profile.RecentCommitments, adaptiveReserveCommit{At: at.UTC(), Percent: percent})
	profile.UnobservedPercent += percent
	profile.UpdatedAt = at.UTC()
	pruneAdaptiveCommitments(profile, at)
	boundAdaptiveCommitments(profile)
	adaptiveReserveRuntime.Unlock()
}

func adaptiveShapeFromProfileKey(key string) adaptiveRequestShape {
	parts := strings.Split(key, "\x1f")
	if len(parts) == 7 {
		return adaptiveRequestShape{
			Multiplier: 1, Provider: parts[1], PhysicalModel: parts[2], ModelFamily: parts[3],
			EffortBucket: parts[4], ContextBucket: parts[5], CostMode: parts[6],
		}
	}
	if len(parts) == 4 {
		return adaptiveRequestShape{
			Multiplier: 1, ModelFamily: parts[1], EffortBucket: parts[2], ContextBucket: parts[3], CostMode: "legacy-unknown",
		}
	}
	return adaptiveRequestShape{
		Multiplier: 1, ModelFamily: "unknown", EffortBucket: "standard", ContextBucket: "small",
	}
}

func captureAdaptiveRefreshWatermark(authIndex string) adaptiveRefreshWatermark {
	authIndex = strings.TrimSpace(authIndex)
	watermark := adaptiveRefreshWatermark{Buckets: make(map[string]float64)}
	allocatorRuntime.Lock()
	watermark.PendingPercent = allocatorRuntime.PendingPercent[authIndex]
	watermark.OrphanPreparedPercent = math.Min(
		watermark.PendingPercent,
		allocatorRuntime.OrphanPreparedPercent[authIndex],
	)
	allocatorRuntime.Unlock()
	allocatorObserveRuntime.Lock()
	watermark.ObservePendingPercent = allocatorObserveRuntime.Accounts[authIndex].Pending
	allocatorObserveRuntime.Unlock()
	adaptiveReserveRuntime.Lock()
	for key, profile := range adaptiveReserveRuntime.Buckets {
		if profile == nil || profile.AuthIndex != authIndex || profile.UnobservedPercent <= 0 {
			continue
		}
		watermark.Buckets[key] = profile.UnobservedPercent
		watermark.Total += profile.UnobservedPercent
	}
	adaptiveReserveRuntime.Unlock()
	return watermark
}

// observeAdaptiveQuotaRefresh closes the feedback loop between predicted
// reservations and the provider-confirmed movement. A 20% real drop after 10%
// of predictions raises future reservations instead of repeating the slip.
func observeAdaptiveQuotaRefresh(
	authIndex string,
	previous, refreshed credentialQuotaState,
	predictedPercent float64,
	at time.Time,
	captured ...adaptiveRefreshWatermark,
) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || quotaConfidence(previous) != "confirmed" || quotaConfidence(refreshed) != "confirmed" {
		return
	}
	previousAt := quotaConfirmedAt(previous)
	if previousAt.IsZero() || !at.After(previousAt) {
		return
	}
	sessionDrop := positiveQuotaDrop(previous.Session.RemainingPercent, refreshed.Session.RemainingPercent)
	weeklyDrop := positiveQuotaDrop(previous.Weekly.RemainingPercent, refreshed.Weekly.RemainingPercent)
	elapsedMinutes := at.Sub(previousAt).Minutes()
	watermark := adaptiveRefreshWatermark{}
	if len(captured) > 0 {
		watermark = captured[0]
	} else {
		watermark = captureAdaptiveRefreshWatermark(authIndex)
	}
	adaptiveReserveRuntime.Lock()
	matching := adaptiveBucketsForAuthLocked(authIndex)
	if len(matching) == 0 || watermark.Total <= 0 {
		profile := ensureAdaptiveProfileLocked(authIndex)
		updateAdaptiveWindow(&profile.Session, sessionDrop, predictedPercent, elapsedMinutes)
		updateAdaptiveWindow(&profile.Weekly, weeklyDrop, predictedPercent, elapsedMinutes)
		profile.LearnedScale = math.Max(profile.Session.LearnedScale, profile.Weekly.LearnedScale)
		profile.ObservedBurnPerMin = math.Max(profile.Session.ObservedBurnPerMin, profile.Weekly.ObservedBurnPerMin)
		profile.UpdatedAt = at.UTC()
		adaptiveReserveRuntime.Unlock()
		stageAdaptiveEstimatorState(authIndex, at)
		return
	}
	totalCaptured := 0.0
	familyCaptured := make(map[string]float64)
	for key, amount := range watermark.Buckets {
		profile := adaptiveReserveRuntime.Buckets[key]
		if profile == nil || profile.AuthIndex != authIndex || amount <= 0 {
			continue
		}
		amount = math.Min(amount, profile.UnobservedPercent)
		totalCaptured += amount
		familyCaptured[profile.Shape.ModelFamily] += amount
	}
	aggregate := ensureAdaptiveProfileLocked(authIndex)
	for key, profile := range adaptiveReserveRuntime.Buckets {
		capturedPercent := watermark.Buckets[key]
		if profile == nil || profile.AuthIndex != authIndex || capturedPercent <= 0 || totalCaptured <= 0 {
			continue
		}
		capturedPercent = math.Min(capturedPercent, profile.UnobservedPercent)
		sessionActual := sessionDrop * capturedPercent / totalCaptured
		weeklyActual, weeklyPredicted := weeklyDrop*capturedPercent/totalCaptured, capturedPercent
		if modelDrop, selected := selectedModelWeeklyDrop(previous, refreshed, profile.Shape.ModelFamily); selected {
			familyTotal := familyCaptured[profile.Shape.ModelFamily]
			if familyTotal > 0 {
				weeklyActual = modelDrop * capturedPercent / familyTotal
			}
		}
		updateAdaptiveWindow(&profile.Session, sessionActual, capturedPercent, elapsedMinutes)
		updateAdaptiveWindow(&profile.Weekly, weeklyActual, weeklyPredicted, elapsedMinutes)
		profile.LearnedScale = math.Max(profile.Session.LearnedScale, profile.Weekly.LearnedScale)
		profile.ObservedBurnPerMin = math.Max(profile.Session.ObservedBurnPerMin, profile.Weekly.ObservedBurnPerMin)
		profile.UnobservedPercent = math.Max(profile.UnobservedPercent-capturedPercent, 0)
		profile.UpdatedAt = at.UTC()
		pruneAdaptiveCommitments(profile, at)
		aggregate.LearnedScale = math.Max(aggregate.LearnedScale, profile.LearnedScale)
		aggregate.ObservedBurnPerMin = math.Max(aggregate.ObservedBurnPerMin, profile.ObservedBurnPerMin)
		aggregate.UpdatedAt = at.UTC()
	}
	unattributedPredicted := math.Max(predictedPercent-totalCaptured, 0)
	// Once bucket weights exist, the provider drop has already been attributed
	// proportionally in full. Predicted percentage and actual provider drop are
	// different units for residual purposes; subtracting one from the other
	// double-counted actual burn whenever actual > predicted. Keep only the
	// truly unattributed prediction as uncertainty and attribute zero additional
	// actual to the compatibility aggregate.
	updateAdaptiveWindow(&aggregate.Session, 0, unattributedPredicted, elapsedMinutes)
	updateAdaptiveWindow(&aggregate.Weekly, 0, unattributedPredicted, elapsedMinutes)
	aggregate.LearnedScale = math.Max(aggregate.Session.LearnedScale, aggregate.Weekly.LearnedScale)
	aggregate.ObservedBurnPerMin = math.Max(aggregate.Session.ObservedBurnPerMin, aggregate.Weekly.ObservedBurnPerMin)
	aggregate.UpdatedAt = at.UTC()
	adaptiveReserveRuntime.Unlock()
	stageAdaptiveEstimatorState(authIndex, at)
}

func updateAdaptiveWindow(window *adaptiveWindowEstimate, actual, predicted, elapsedMinutes float64) {
	if window == nil {
		return
	}
	if actual <= 0 {
		window.StableIntervals++
		if window.StableIntervals >= 3 {
			window.ObservedBurnPerMin *= 0.5
			window.LearnedScale = math.Max(1, 1+(math.Max(window.LearnedScale, 1)-1)*0.75)
		}
		return
	}
	window.StableIntervals = 0
	rate := actual / math.Max(elapsedMinutes, 1.0/60)
	if window.ObservedBurnPerMin <= 0 {
		window.ObservedBurnPerMin = rate
	} else {
		window.ObservedBurnPerMin = window.ObservedBurnPerMin*0.65 + rate*0.35
	}
	if predicted <= 0 {
		return
	}
	ratio := math.Min(math.Max(actual/predicted, 1), adaptiveMaximumLearnedScale)
	if window.LearnedScale <= 0 {
		window.LearnedScale = ratio
		return
	}
	weight := 0.35
	if ratio > window.LearnedScale {
		weight = 0.65
	}
	window.LearnedScale = math.Min(math.Max(window.LearnedScale*(1-weight)+ratio*weight, 1), adaptiveMaximumLearnedScale)
}

func selectedModelWeeklyDrop(previous, refreshed credentialQuotaState, family string) (float64, bool) {
	previousWindow, previousSelected := selectedModelWeeklyWindow(previous, family)
	refreshedWindow, refreshedSelected := selectedModelWeeklyWindow(refreshed, family)
	if !previousSelected || !refreshedSelected {
		return 0, false
	}
	return positiveQuotaDrop(previousWindow.RemainingPercent, refreshedWindow.RemainingPercent), true
}

func selectedModelWeeklyWindow(quota credentialQuotaState, family string) (quotaWindowState, bool) {
	selected := quotaWindowState{}
	found := false
	for _, candidate := range quota.ModelWeekly {
		if !quotaModelMatches(family, candidate.Model) {
			continue
		}
		window := normalizeQuotaWindow(candidate.quotaWindowState)
		if !found || window.RemainingPercent < selected.RemainingPercent {
			selected, found = window, true
		}
	}
	return selected, found
}

func positiveQuotaDrop(previous, current float64) float64 {
	if current >= previous {
		return 0
	}
	return previous - current
}

// adaptiveExposureGuard forecasts the unconfirmed burn through the next
// scheduled provider refresh. It is local-only and applies at every headroom;
// near-floor routing is not the switch that turns safety on.
func adaptiveExposureGuard(authIndex, profileKey string, quota credentialQuotaState, window string, cfg pluginConfig, now time.Time) float64 {
	adaptiveReserveRuntime.Lock()
	authIndex = strings.TrimSpace(authIndex)
	if adaptiveReserveRuntime.SaturationGlobal {
		adaptiveReserveRuntime.Unlock()
		return 100
	}
	if _, saturated := adaptiveReserveRuntime.Saturated[authIndex]; saturated {
		adaptiveReserveRuntime.Unlock()
		return 100
	}
	observedRate := 0.0
	if profile := adaptiveReserveRuntime.Buckets[profileKey]; profile != nil {
		if window == adaptiveWindowWeekly {
			observedRate = profile.Weekly.ObservedBurnPerMin
		} else {
			observedRate = profile.Session.ObservedBurnPerMin
		}
	}
	if observedRate <= 0 {
		if aggregate := adaptiveReserveRuntime.Profiles[strings.TrimSpace(authIndex)]; aggregate != nil {
			observedRate = math.Max(observedRate, aggregate.ObservedBurnPerMin)
		}
		if overflow := adaptiveReserveRuntime.Overflow[strings.TrimSpace(authIndex)]; overflow != nil {
			observedRate = math.Max(observedRate, overflow.ObservedBurnPerMin)
		}
	}
	adaptiveReserveRuntime.Unlock()
	if observedRate <= 0 {
		return adaptiveColdStartExposureGuard(cfg, quota, now)
	}
	exposure := adaptiveSnapshotExposure(cfg, quota, now)
	return math.Min(math.Max(observedRate*exposure.Minutes(), 0), 100)
}

func adaptiveColdStartExposureGuard(cfg pluginConfig, quota credentialQuotaState, now time.Time) float64 {
	confirmedAt := quotaConfirmedAt(quota)
	if confirmedAt.IsZero() || !now.After(confirmedAt) {
		return 0.01
	}
	refresh := time.Duration(cfg.QuotaUsageRefreshSeconds) * time.Second
	if refresh <= 0 {
		refresh = time.Duration(cfg.QuotaRefreshSeconds) * time.Second
	}
	if refresh <= 0 {
		refresh = time.Minute
	}
	age := now.Sub(confirmedAt)
	if age >= refresh {
		// With no learned burn, a due snapshot has no evidence that external
		// clients left the LKG untouched. Block secondary borrowing until the
		// coalesced background refresh publishes new confirmation.
		return 100
	}
	progress := math.Min(math.Max(float64(age)/float64(refresh), 0), 1)
	return math.Max(0.01, 100*progress*progress)
}

func ensureAdaptiveProfileLocked(authIndex string) *adaptiveReserveProfile {
	profile := adaptiveReserveRuntime.Profiles[authIndex]
	if profile == nil {
		for len(adaptiveReserveRuntime.Profiles) >= adaptiveMaximumProfileEntries {
			if !evictOldestAdaptiveAggregateLocked() {
				break
			}
		}
		profile = &adaptiveReserveProfile{AuthIndex: authIndex, LearnedScale: 1, UpdatedAt: time.Now().UTC()}
		adaptiveReserveRuntime.Profiles[authIndex] = profile
	}
	return profile
}

func evictOldestAdaptiveAggregateLocked() bool {
	oldestKey := ""
	oldestAt := time.Time{}
	for key, profile := range adaptiveReserveRuntime.Profiles {
		if profile == nil {
			continue
		}
		if oldestKey == "" || profile.UpdatedAt.Before(oldestAt) {
			oldestKey, oldestAt = key, profile.UpdatedAt
		}
	}
	if oldestKey == "" {
		return false
	}
	source := adaptiveReserveRuntime.Profiles[oldestKey]
	overflow := ensureAdaptiveOverflowLocked(oldestKey)
	if overflow != nil {
		overflow.LearnedScale = math.Max(overflow.LearnedScale, source.LearnedScale)
		overflow.ObservedBurnPerMin = math.Max(overflow.ObservedBurnPerMin, source.ObservedBurnPerMin)
		overflow.UnobservedPercent += source.UnobservedPercent
		overflow.UpdatedAt = time.Now().UTC()
	}
	delete(adaptiveReserveRuntime.Profiles, oldestKey)
	return true
}

func ensureAdaptiveOverflowLocked(authIndex string) *adaptiveReserveProfile {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil
	}
	if profile := adaptiveReserveRuntime.Overflow[authIndex]; profile != nil {
		delete(adaptiveReserveRuntime.Saturated, authIndex)
		return profile
	}
	for len(adaptiveReserveRuntime.Overflow) >= adaptiveMaximumOverflowEntries {
		oldestKey := ""
		oldestAt := time.Time{}
		for key, profile := range adaptiveReserveRuntime.Overflow {
			if profile == nil || oldestKey == "" || profile.UpdatedAt.Before(oldestAt) {
				oldestKey = key
				if profile != nil {
					oldestAt = profile.UpdatedAt
				}
			}
		}
		if oldestKey == "" {
			return nil
		}
		markAdaptiveSaturatedLocked(oldestKey, time.Now().UTC())
		delete(adaptiveReserveRuntime.Overflow, oldestKey)
	}
	profile := &adaptiveReserveProfile{AuthIndex: authIndex, LearnedScale: 1, UpdatedAt: time.Now().UTC()}
	adaptiveReserveRuntime.Overflow[authIndex] = profile
	delete(adaptiveReserveRuntime.Saturated, authIndex)
	return profile
}

func markAdaptiveSaturatedLocked(authIndex string, now time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || adaptiveReserveRuntime.SaturationGlobal {
		return
	}
	if adaptiveReserveRuntime.Saturated == nil {
		adaptiveReserveRuntime.Saturated = make(map[string]time.Time)
	}
	if _, exists := adaptiveReserveRuntime.Saturated[authIndex]; exists || len(adaptiveReserveRuntime.Saturated) < adaptiveMaximumOverflowEntries {
		adaptiveReserveRuntime.Saturated[authIndex] = now.UTC()
		return
	}
	// Exact uncertainty markers are also bounded. Once exhausted, retaining
	// isolation is impossible without forgetting an older credential, so all
	// secondary borrowing fails closed. Primary traffic does not consult this
	// guard and remains available.
	adaptiveReserveRuntime.SaturationGlobal = true
}

func adaptiveEstimatorReadyForReset() error {
	adaptiveReserveRuntime.Lock()
	defer adaptiveReserveRuntime.Unlock()
	for _, profile := range adaptiveReserveRuntime.Buckets {
		if profile != nil && profile.UnobservedPercent > 0 {
			return fmt.Errorf("adaptive estimator still contains unobserved bucket work")
		}
	}
	for _, profile := range adaptiveReserveRuntime.Profiles {
		if profile != nil && profile.UnobservedPercent > 0 {
			return fmt.Errorf("adaptive estimator still contains unobserved aggregate work")
		}
	}
	for _, profile := range adaptiveReserveRuntime.Overflow {
		if profile != nil && profile.UnobservedPercent > 0 {
			return fmt.Errorf("adaptive estimator overflow still contains unobserved work")
		}
	}
	return nil
}

// adaptiveEstimatorIdentitySaturated is the common fail-closed identity gate
// for exceptional routes such as /compact. The normal secondary allocator also
// observes these markers through adaptiveExposureGuard, but exceptional routes
// must reject them explicitly before installing any provider-side lease.
func adaptiveEstimatorIdentitySaturated(authIndex string) bool {
	authIndex = strings.TrimSpace(authIndex)
	adaptiveReserveRuntime.Lock()
	defer adaptiveReserveRuntime.Unlock()
	if adaptiveReserveRuntime.SaturationGlobal {
		return true
	}
	_, saturated := adaptiveReserveRuntime.Saturated[authIndex]
	return saturated
}

func resetAdaptiveEstimatorSaturationAfterReconciliation() {
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Saturated = make(map[string]time.Time)
	adaptiveReserveRuntime.SaturationGlobal = false
	adaptiveReserveRuntime.Unlock()
}

func ensureAdaptiveBucketLocked(key, authIndex string, shape adaptiveRequestShape) *adaptiveReserveProfile {
	profile := adaptiveReserveRuntime.Buckets[key]
	if profile == nil {
		pruneAdaptiveProfilesLocked(time.Now().UTC())
		for len(adaptiveReserveRuntime.Buckets) >= adaptiveMaximumProfileEntries {
			if !evictOldestAdaptiveBucketLocked() {
				break
			}
		}
		profile = &adaptiveReserveProfile{
			AuthIndex: authIndex, Shape: shape,
			Session:      adaptiveWindowEstimate{LearnedScale: 1},
			Weekly:       adaptiveWindowEstimate{LearnedScale: 1},
			LearnedScale: 1, UpdatedAt: time.Now().UTC(),
		}
		adaptiveReserveRuntime.Buckets[key] = profile
	}
	return profile
}

func pruneAdaptiveProfilesLocked(now time.Time) {
	for key, profile := range adaptiveReserveRuntime.Buckets {
		if profile != nil && profile.UnobservedPercent <= 0 && !profile.UpdatedAt.IsZero() && now.Sub(profile.UpdatedAt) > adaptiveProfileStateTTL {
			delete(adaptiveReserveRuntime.Buckets, key)
		}
	}
	for key, profile := range adaptiveReserveRuntime.Profiles {
		if profile != nil && profile.UnobservedPercent <= 0 && !profile.UpdatedAt.IsZero() && now.Sub(profile.UpdatedAt) > adaptiveProfileStateTTL {
			delete(adaptiveReserveRuntime.Profiles, key)
		}
	}
	for key, profile := range adaptiveReserveRuntime.Overflow {
		if profile != nil && profile.UnobservedPercent <= 0 && !profile.UpdatedAt.IsZero() && now.Sub(profile.UpdatedAt) > adaptiveProfileStateTTL {
			delete(adaptiveReserveRuntime.Overflow, key)
		}
	}
	// Saturation is a safety fact, not a cache entry. Exact and global markers
	// survive indefinitely and are cleared only by the authenticated reconciler
	// after all unobserved work has been proven resolved.
}

func evictOldestAdaptiveBucketLocked() bool {
	oldestKey := ""
	oldestAt := time.Time{}
	for key, profile := range adaptiveReserveRuntime.Buckets {
		if profile == nil || oldestKey == "" || profile.UpdatedAt.Before(oldestAt) {
			oldestKey = key
			if profile != nil {
				oldestAt = profile.UpdatedAt
			}
		}
	}
	if oldestKey == "" {
		return false
	}
	profile := adaptiveReserveRuntime.Buckets[oldestKey]
	if profile != nil {
		aggregate := ensureAdaptiveProfileLocked(profile.AuthIndex)
		aggregate.LearnedScale = math.Max(aggregate.LearnedScale, profile.LearnedScale)
		aggregate.ObservedBurnPerMin = math.Max(aggregate.ObservedBurnPerMin, profile.ObservedBurnPerMin)
		aggregate.UnobservedPercent += profile.UnobservedPercent
		aggregate.UpdatedAt = time.Now().UTC()
	}
	delete(adaptiveReserveRuntime.Buckets, oldestKey)
	return true
}

func adaptiveBucketsForAuthLocked(authIndex string) []*adaptiveReserveProfile {
	values := make([]*adaptiveReserveProfile, 0)
	for _, profile := range adaptiveReserveRuntime.Buckets {
		if profile != nil && profile.AuthIndex == authIndex {
			values = append(values, profile)
		}
	}
	return values
}

func pruneAdaptiveCommitments(profile *adaptiveReserveProfile, now time.Time) {
	if profile == nil || len(profile.RecentCommitments) == 0 {
		return
	}
	cutoff := now.UTC().Add(-adaptiveCommitHorizon)
	kept := profile.RecentCommitments[:0]
	for _, item := range profile.RecentCommitments {
		if !item.At.Before(cutoff) {
			kept = append(kept, item)
		}
	}
	profile.RecentCommitments = kept
}

func boundAdaptiveCommitments(profile *adaptiveReserveProfile) {
	if profile == nil || len(profile.RecentCommitments) <= adaptiveMaximumRecentCommitments {
		return
	}
	mergeCount := len(profile.RecentCommitments) - adaptiveMaximumRecentCommitments + 1
	merged := adaptiveReserveCommit{}
	for _, item := range profile.RecentCommitments[:mergeCount] {
		merged.Percent += item.Percent
		if item.At.After(merged.At) {
			merged.At = item.At
		}
	}
	kept := make([]adaptiveReserveCommit, 1, adaptiveMaximumRecentCommitments)
	kept[0] = merged
	kept = append(kept, profile.RecentCommitments[mergeCount:]...)
	profile.RecentCommitments = kept
}

func resetAdaptiveReserveForTest() {
	adaptiveRoutingSaturated.Store(false)
	adaptiveReserveRuntime.Lock()
	adaptiveReserveRuntime.Profiles = make(map[string]*adaptiveReserveProfile)
	adaptiveReserveRuntime.Buckets = make(map[string]*adaptiveReserveProfile)
	adaptiveReserveRuntime.Overflow = make(map[string]*adaptiveReserveProfile)
	adaptiveReserveRuntime.Saturated = make(map[string]time.Time)
	adaptiveReserveRuntime.SaturationGlobal = false
	adaptiveReserveRuntime.Unlock()
	allocatorRuntime.Lock()
	allocatorRuntime.InFlightPercent = make(map[string]float64)
	allocatorRuntime.PendingPercent = make(map[string]float64)
	allocatorRuntime.OrphanPreparedPercent = make(map[string]float64)
	allocatorRuntime.InFlightRequests = make(map[string]int)
	allocatorRuntime.PendingRequests = make(map[string]int)
	allocatorRuntime.Unlock()
	resetAllocatorObserveRuntime()
	resetAdaptiveStatusRuntime()
}
