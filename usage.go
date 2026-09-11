package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hostAuthHTTPMethod   = "host.auth.http.do"
	quotaRefreshInterval = time.Minute
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
)

type usageRequest struct {
	APIKey  string `json:"api_key"`
	Group   string `json:"group,omitempty"`
	Refresh bool   `json:"refresh"`
}

type usageResponse struct {
	Success       bool           `json:"success"`
	Groups        []string       `json:"groups,omitempty"`
	Accounts      []usageAccount `json:"accounts,omitempty"`
	UpdatedAt     int64          `json:"updated_at,omitempty"`
	NextRefreshAt int64          `json:"next_refresh_at,omitempty"`
	Cached        bool           `json:"cached"`
	Stale         bool           `json:"stale,omitempty"`
	RefreshError  string         `json:"refresh_error,omitempty"`
	Error         string         `json:"error,omitempty"`
}

type usageAccount struct {
	Name       string           `json:"name"`
	Plan       string           `json:"plan,omitempty"`
	RateLimits []usageRateLimit `json:"rate_limits"`
}

type usageRateLimit struct {
	Window    string  `json:"window"`
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	ResetAt   string  `json:"reset_at,omitempty"`
	Unit      string  `json:"unit,omitempty"`
}

type officialUsagePayload struct {
	PlanType  string                 `json:"plan_type"`
	RateLimit *officialRateLimitInfo `json:"rate_limit"`
}

type officialRateLimitInfo struct {
	PrimaryWindow   *officialUsageWindow `json:"primary_window"`
	SecondaryWindow *officialUsageWindow `json:"secondary_window"`
}

type officialUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

type hostAuthHTTPRequest struct {
	AuthIndex string          `json:"auth_index"`
	Request   hostHTTPRequest `json:"request"`
}

type hostHTTPRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
}

type hostHTTPResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}

type quotaCacheEntry struct {
	Account   usageAccount
	UpdatedAt time.Time
}

type quotaFetchResult struct {
	Account       usageAccount
	UpdatedAt     time.Time
	NextRefreshAt time.Time
	Cached        bool
	Stale         bool
	Err           error
}

type quotaFlight struct {
	done chan struct{}
}

var quotaUsageRuntime = struct {
	sync.Mutex
	cache       map[string]quotaCacheEntry
	attemptedAt map[string]time.Time
	failures    map[string]error
	flights     map[string]*quotaFlight
}{
	cache:       make(map[string]quotaCacheEntry),
	attemptedAt: make(map[string]time.Time),
	failures:    make(map[string]error),
	flights:     make(map[string]*quotaFlight),
}

func usageForAPIKey(req usageRequest) (int, any) {
	policy, ok := policyForAPIKey(req.APIKey)
	if !ok {
		return http.StatusUnauthorized, usageResponse{Success: false, Error: "unauthorized"}
	}
	requestedGroup := strings.TrimSpace(req.Group)
	responseGroups := append([]string(nil), policy.Groups...)
	if requestedGroup != "" {
		if len(policy.Groups) > 0 && !containsString(policy.Groups, "*") && !containsString(policy.Groups, requestedGroup) {
			return http.StatusForbidden, usageResponse{Success: false, Groups: policy.Groups, Error: "group_not_allowed"}
		}
		responseGroups = []string{requestedGroup}
	}
	if errRefresh := refreshAuthCache(false); errRefresh != nil && len(cachedAuthSnapshot()) == 0 {
		return http.StatusBadGateway, usageResponse{Success: false, Error: errRefresh.Error()}
	}

	candidates := make([]cachedAuth, 0)
	for _, auth := range cachedAuthSnapshot() {
		provider := firstNonEmpty(auth.Entry.Provider, auth.Entry.Type)
		if auth.Entry.Disabled || !strings.EqualFold(provider, "codex") || !policyAllowsAuth(policy, auth.Groups) {
			continue
		}
		if requestedGroup != "" && !containsString(auth.Groups, requestedGroup) {
			continue
		}
		candidates = append(candidates, auth)
	}
	if len(candidates) == 0 {
		return http.StatusNotFound, usageResponse{Success: false, Groups: responseGroups, Error: "no_matching_codex_auth"}
	}

	accounts := make([]usageAccount, 0, len(candidates))
	refreshErrors := make([]error, 0)
	allCached := true
	stale := false
	updatedAt := time.Time{}
	nextRefreshAt := time.Time{}
	for index, auth := range candidates {
		result := quotaUsageForAuth(auth, req.Refresh, index+1)
		if result.Err != nil {
			refreshErrors = append(refreshErrors, result.Err)
		}
		if len(result.Account.RateLimits) == 0 {
			continue
		}
		accounts = append(accounts, result.Account)
		allCached = allCached && result.Cached
		stale = stale || result.Stale
		if updatedAt.IsZero() || result.UpdatedAt.Before(updatedAt) {
			updatedAt = result.UpdatedAt
		}
		if result.NextRefreshAt.After(nextRefreshAt) {
			nextRefreshAt = result.NextRefreshAt
		}
	}
	if len(accounts) == 0 {
		message := "official quota unavailable"
		if len(refreshErrors) > 0 {
			message = errors.Join(refreshErrors...).Error()
		}
		return http.StatusBadGateway, usageResponse{Success: false, Groups: responseGroups, Error: message}
	}

	sort.SliceStable(accounts, func(i, j int) bool {
		return strings.ToLower(accounts[i].Name) < strings.ToLower(accounts[j].Name)
	})
	response := usageResponse{
		Success:       true,
		Groups:        responseGroups,
		Accounts:      accounts,
		UpdatedAt:     updatedAt.Unix(),
		NextRefreshAt: nextRefreshAt.Unix(),
		Cached:        allCached,
		Stale:         stale || len(refreshErrors) > 0,
	}
	if len(refreshErrors) > 0 {
		response.RefreshError = errors.Join(refreshErrors...).Error()
	}
	return http.StatusOK, response
}

func quotaUsageForAuth(auth cachedAuth, refresh bool, ordinal int) quotaFetchResult {
	authIndex := strings.TrimSpace(auth.Entry.AuthIndex)
	if authIndex == "" {
		return quotaFetchResult{Err: errors.New("Codex credential is missing auth_index")}
	}
	for {
		now := time.Now().UTC()
		quotaUsageRuntime.Lock()
		cached, hasCache := quotaUsageRuntime.cache[authIndex]
		attemptedAt := quotaUsageRuntime.attemptedAt[authIndex]
		lastFailure := quotaUsageRuntime.failures[authIndex]
		if hasCache && !refresh {
			next := attemptedAt.Add(quotaRefreshInterval)
			if next.Before(now) {
				next = now
			}
			quotaUsageRuntime.Unlock()
			return quotaFetchResult{Account: cached.Account, UpdatedAt: cached.UpdatedAt, NextRefreshAt: next, Cached: true, Stale: lastFailure != nil, Err: lastFailure}
		}
		if !attemptedAt.IsZero() && now.Sub(attemptedAt) < quotaRefreshInterval {
			next := attemptedAt.Add(quotaRefreshInterval)
			quotaUsageRuntime.Unlock()
			if hasCache {
				return quotaFetchResult{Account: cached.Account, UpdatedAt: cached.UpdatedAt, NextRefreshAt: next, Cached: true, Stale: lastFailure != nil, Err: lastFailure}
			}
			return quotaFetchResult{NextRefreshAt: next, Cached: true, Stale: lastFailure != nil, Err: lastFailure}
		}
		if flight := quotaUsageRuntime.flights[authIndex]; flight != nil {
			done := flight.done
			quotaUsageRuntime.Unlock()
			<-done
			continue
		}
		flight := &quotaFlight{done: make(chan struct{})}
		quotaUsageRuntime.flights[authIndex] = flight
		quotaUsageRuntime.attemptedAt[authIndex] = now
		quotaUsageRuntime.Unlock()

		account, errFetch := fetchOfficialQuota(auth, ordinal)
		completedAt := time.Now().UTC()
		quotaUsageRuntime.Lock()
		delete(quotaUsageRuntime.flights, authIndex)
		if errFetch == nil {
			cached = quotaCacheEntry{Account: account, UpdatedAt: completedAt}
			quotaUsageRuntime.cache[authIndex] = cached
			delete(quotaUsageRuntime.failures, authIndex)
			hasCache = true
		} else {
			quotaUsageRuntime.failures[authIndex] = errFetch
		}
		close(flight.done)
		quotaUsageRuntime.Unlock()

		next := now.Add(quotaRefreshInterval)
		if errFetch == nil {
			return quotaFetchResult{Account: account, UpdatedAt: completedAt, NextRefreshAt: next}
		}
		if hasCache {
			return quotaFetchResult{Account: cached.Account, UpdatedAt: cached.UpdatedAt, NextRefreshAt: next, Cached: true, Stale: true, Err: errFetch}
		}
		return quotaFetchResult{NextRefreshAt: next, Stale: true, Err: errFetch}
	}
}

func fetchOfficialQuota(auth cachedAuth, ordinal int) (usageAccount, error) {
	headers := http.Header{
		"Authorization": []string{"Bearer $TOKEN$"},
		"Content-Type":  []string{"application/json"},
		"User-Agent":    []string{"codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"},
	}
	if accountID := strings.TrimSpace(auth.ChatGPTAccount); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	var response hostHTTPResponse
	if errCall := callHost(hostAuthHTTPMethod, hostAuthHTTPRequest{
		AuthIndex: auth.Entry.AuthIndex,
		Request: hostHTTPRequest{
			Method:  http.MethodGet,
			URL:     codexUsageURL,
			Headers: headers,
		},
	}, &response); errCall != nil {
		return usageAccount{}, fmt.Errorf("%s quota refresh failed: %w", usageAccountName(auth, ordinal), errCall)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return usageAccount{}, fmt.Errorf("%s quota refresh returned HTTP %d", usageAccountName(auth, ordinal), response.StatusCode)
	}
	var payload officialUsagePayload
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		return usageAccount{}, fmt.Errorf("%s quota response decode failed: %w", usageAccountName(auth, ordinal), errUnmarshal)
	}
	limits := officialRateLimits(payload.RateLimit)
	if len(limits) == 0 {
		return usageAccount{}, fmt.Errorf("%s quota response has no supported window", usageAccountName(auth, ordinal))
	}
	return usageAccount{
		Name:       usageAccountName(auth, ordinal),
		Plan:       firstNonEmpty(payload.PlanType, auth.PlanType),
		RateLimits: limits,
	}, nil
}

func officialRateLimits(rateLimit *officialRateLimitInfo) []usageRateLimit {
	if rateLimit == nil {
		return nil
	}
	primaryAvailable := rateLimit.PrimaryWindow != nil && rateLimit.PrimaryWindow.UsedPercent != nil
	secondaryAvailable := rateLimit.SecondaryWindow != nil && rateLimit.SecondaryWindow.UsedPercent != nil
	limits := make([]usageRateLimit, 0, 2)
	if primaryAvailable {
		fallback := "7d"
		if secondaryAvailable {
			fallback = "5h"
		}
		limits = append(limits, officialRateLimit(rateLimit.PrimaryWindow, fallback))
	}
	if secondaryAvailable {
		limits = append(limits, officialRateLimit(rateLimit.SecondaryWindow, "7d"))
	}
	return limits
}

func officialRateLimit(window *officialUsageWindow, fallback string) usageRateLimit {
	used := max(0, min(100, *window.UsedPercent))
	resetAt := ""
	if window.ResetAt != nil && *window.ResetAt > 0 {
		resetAt = time.Unix(*window.ResetAt, 0).UTC().Format(time.RFC3339)
	}
	return usageRateLimit{
		Window:    officialQuotaWindow(window.LimitWindowSeconds, fallback),
		Limit:     100,
		Used:      used,
		Remaining: 100 - used,
		ResetAt:   resetAt,
		Unit:      "percent",
	}
}

func officialQuotaWindow(seconds int64, fallback string) string {
	switch {
	case seconds >= int64(4*time.Hour/time.Second) && seconds <= int64(6*time.Hour/time.Second):
		return "5h"
	case seconds >= int64(6*24*time.Hour/time.Second) && seconds <= int64(8*24*time.Hour/time.Second):
		return "7d"
	default:
		return fallback
	}
}

func usageAccountName(auth cachedAuth, ordinal int) string {
	for _, candidate := range []string{auth.Entry.Label, auth.Entry.Email, auth.Entry.Account} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.Contains(candidate, "@") {
			if masked := maskUsageEmail(candidate); masked != "" {
				return masked
			}
		}
		return candidate
	}
	return fmt.Sprintf("Codex %d", ordinal)
}

func maskUsageEmail(value string) string {
	parts := strings.Split(strings.TrimSpace(value), "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	local := []rune(parts[0])
	return string(local[0]) + "***@" + parts[1]
}
