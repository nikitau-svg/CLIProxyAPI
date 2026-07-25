package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type allocatorRuntimeState struct {
	sync.Mutex
	InFlightPercent map[string]float64
	PendingPercent  map[string]float64
}

var allocatorRuntime = allocatorRuntimeState{
	InFlightPercent: make(map[string]float64),
	PendingPercent:  make(map[string]float64),
}

func authenticatedExecutionProject(req rpcExecutorRequest, cfg pluginConfig) (smartKeyConfig, bool) {
	if project, ok := smartKeyFromMetadata(req.Metadata, cfg); ok {
		return project, true
	}
	if plaintext := requestCredential(req.Headers, req.Query); plaintext != "" {
		return matchSmartKey(cfg, plaintext)
	}
	return smartKeyConfig{}, false
}

func allocateCandidateAuths(
	req rpcExecutorRequest,
	cfg pluginConfig,
	project smartKeyConfig,
	item candidate,
	auths []pluginapi.HostAuthFileEntry,
	sticky string,
) []executionAttempt {
	primaryIndexes := resolvedPrimaryAuthIndexes(project.PrimaryAuthIDs, auths)
	primary := make([]executionAttempt, 0, len(auths))
	secondary := make([]executionAttempt, 0, len(auths))
	for _, auth := range auths {
		authIndex := strings.TrimSpace(auth.AuthIndex)
		subscription := subscriptionPolicy(cfg, authIndex)
		if !subscriptionEnabled(subscription) {
			continue
		}
		quota := refreshQuotaIfNeeded(req.HostCallbackID, auth, false)
		tariff := effectiveTariff(cfg, subscription, firstNonEmpty(auth.Provider, auth.Type), quota)
		_, isPrimary := primaryIndexes[authIndex]
		attempt := executionAttempt{
			LogicalModel:       "",
			Candidate:          item,
			Auth:               auth,
			ProjectID:          project.ID,
			Primary:            isPrimary,
			AllocatorManaged:   cfg.AllocatorMode == "enforce",
			ReservationPercent: tariff.ReservationPercent,
			TariffID:           tariff.ID,
		}
		if isPrimary {
			primary = append(primary, attempt)
			continue
		}
		if secondaryQuotaEligible(cfg, quota, item.Model, tariff, authIndex, tariff.ReservationPercent) {
			secondary = append(secondary, attempt)
		}
	}

	orderAuthAttempts(primary, sticky)
	sort.SliceStable(secondary, func(i, j int) bool {
		left := allocatorStress(cfg, secondary[i])
		right := allocatorStress(cfg, secondary[j])
		if math.Abs(left-right) > 0.000001 {
			return left < right
		}
		leftTie := rendezvousScore(sticky, item.Provider, item.Model, stableAuthIndex(secondary[i].Auth))
		rightTie := rendezvousScore(sticky, item.Provider, item.Model, stableAuthIndex(secondary[j].Auth))
		if leftTie == rightTie {
			return stableAuthIndex(secondary[i].Auth) < stableAuthIndex(secondary[j].Auth)
		}
		return leftTie > rightTie
	})
	return append(primary, secondary...)
}

func resolvedPrimaryAuthIndexes(configured []string, auths []pluginapi.HostAuthFileEntry) map[string]struct{} {
	resolved := make(map[string]struct{}, len(configured))
	for _, raw := range configured {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		for _, auth := range auths {
			if strings.TrimSpace(auth.AuthIndex) == value {
				resolved[strings.TrimSpace(auth.AuthIndex)] = struct{}{}
				goto nextPrimary
			}
		}
		for _, auth := range auths {
			if strings.TrimSpace(auth.ID) == value {
				resolved[strings.TrimSpace(auth.AuthIndex)] = struct{}{}
				goto nextPrimary
			}
		}
		for _, auth := range auths {
			if strings.TrimSpace(auth.Name) == value {
				resolved[strings.TrimSpace(auth.AuthIndex)] = struct{}{}
				goto nextPrimary
			}
		}
	nextPrimary:
	}
	return resolved
}

func orderAuthAttempts(attempts []executionAttempt, sticky string) {
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].Auth.Priority != attempts[j].Auth.Priority {
			return attempts[i].Auth.Priority > attempts[j].Auth.Priority
		}
		left := rendezvousScore(sticky, attempts[i].Candidate.Provider, attempts[i].Candidate.Model, stableAuthIndex(attempts[i].Auth))
		right := rendezvousScore(sticky, attempts[j].Candidate.Provider, attempts[j].Candidate.Model, stableAuthIndex(attempts[j].Auth))
		if left == right {
			return stableAuthIndex(attempts[i].Auth) < stableAuthIndex(attempts[j].Auth)
		}
		return left > right
	})
}

func stableAuthIndex(auth pluginapi.HostAuthFileEntry) string {
	if value := strings.TrimSpace(auth.AuthIndex); value != "" {
		return value
	}
	return authIdentifier(auth)
}

func secondaryQuotaEligible(
	cfg pluginConfig,
	quota credentialQuotaState,
	model string,
	tariff tariffConfig,
	authIndex string,
	reservation float64,
) bool {
	if quotaConfidence(quota) != "confirmed" {
		return cfg.UnknownSecondaryPolicy == "allow"
	}
	session, weekly := effectiveQuotaWindows(quota, model)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[strings.TrimSpace(authIndex)] +
		allocatorRuntime.PendingPercent[strings.TrimSpace(authIndex)]
	allocatorRuntime.Unlock()
	return session.RemainingPercent-reserved-reservation > tariff.SessionFloorPercent &&
		weekly.RemainingPercent-reserved-reservation > tariff.WeeklyFloorPercent
}

func allocatorStress(cfg pluginConfig, attempt executionAttempt) float64 {
	quota := quotaSnapshot(attempt.Auth.AuthIndex)
	tariff := tariffByID(cfg, attempt.TariffID)
	session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
	minHeadroom := 1.0
	if quotaConfidence(quota) == "confirmed" {
		sessionHeadroom := normalizedHeadroom(session.RemainingPercent, tariff.SessionFloorPercent)
		weeklyHeadroom := normalizedHeadroom(weekly.RemainingPercent, tariff.WeeklyFloorPercent)
		minHeadroom = math.Min(sessionHeadroom, weeklyHeadroom)
	}
	usage := authUsageSummary(attempt.Auth.AuthIndex, time.Now())
	usagePressure := math.Log1p(float64(usage.Weekly.TotalTokens)) / math.Max(tariff.Multiplier, 1)
	allocatorRuntime.Lock()
	reserved := allocatorRuntime.InFlightPercent[strings.TrimSpace(attempt.Auth.AuthIndex)] +
		allocatorRuntime.PendingPercent[strings.TrimSpace(attempt.Auth.AuthIndex)]
	allocatorRuntime.Unlock()
	return (1-minHeadroom)*100 + usagePressure + reserved
}

func normalizedHeadroom(remaining, floor float64) float64 {
	if remaining <= floor {
		return 0
	}
	if floor >= 100 {
		return 0
	}
	return math.Min((remaining-floor)/(100-floor), 1)
}

func acquireAttemptLease(attempt executionAttempt) (func(bool), bool) {
	if !attempt.AllocatorManaged || strings.TrimSpace(attempt.Auth.AuthIndex) == "" {
		return func(bool) {}, true
	}
	cfg := loadedConfig()
	authIndex := strings.TrimSpace(attempt.Auth.AuthIndex)
	allocatorRuntime.Lock()
	quota := quotaSnapshot(authIndex)
	tariff := tariffByID(cfg, attempt.TariffID)
	if quotaConfidence(quota) != "confirmed" {
		// An unknown snapshot only blocks secondaries; a pinned primary is
		// still trusted while quota discovery catches up.
		if !attempt.Primary && cfg.UnknownSecondaryPolicy != "allow" {
			allocatorRuntime.Unlock()
			return func(bool) {}, false
		}
	} else {
		session, weekly := effectiveQuotaWindows(quota, attempt.Candidate.Model)
		reserved := allocatorRuntime.InFlightPercent[authIndex] + allocatorRuntime.PendingPercent[authIndex]
		// Being primary grants priority and the right to spend the reserve
		// below the configured floor — it is not an exemption from the quota
		// itself. Without this a pinned credential keeps being retried at 0%
		// remaining, which only produces upstream rate limits.
		sessionFloor, weeklyFloor := tariff.SessionFloorPercent, tariff.WeeklyFloorPercent
		if attempt.Primary {
			sessionFloor, weeklyFloor = 0, 0
		}
		if session.RemainingPercent-reserved-attempt.ReservationPercent <= sessionFloor ||
			weekly.RemainingPercent-reserved-attempt.ReservationPercent <= weeklyFloor {
			allocatorRuntime.Unlock()
			return func(bool) {}, false
		}
	}
	allocatorRuntime.InFlightPercent[authIndex] += attempt.ReservationPercent
	allocatorRuntime.Unlock()

	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			allocatorRuntime.Lock()
			allocatorRuntime.InFlightPercent[authIndex] -= attempt.ReservationPercent
			if allocatorRuntime.InFlightPercent[authIndex] <= 0 {
				delete(allocatorRuntime.InFlightPercent, authIndex)
			}
			if commit {
				allocatorRuntime.PendingPercent[authIndex] += attempt.ReservationPercent
			}
			allocatorRuntime.Unlock()
		})
	}, true
}

func pendingReservationPercent(authIndex string) float64 {
	allocatorRuntime.Lock()
	defer allocatorRuntime.Unlock()
	return allocatorRuntime.PendingPercent[strings.TrimSpace(authIndex)]
}

func clearPendingReservation(authIndex string, amount float64) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || amount <= 0 {
		return
	}
	allocatorRuntime.Lock()
	allocatorRuntime.PendingPercent[authIndex] -= amount
	if allocatorRuntime.PendingPercent[authIndex] <= 0 {
		delete(allocatorRuntime.PendingPercent, authIndex)
	}
	allocatorRuntime.Unlock()
}
