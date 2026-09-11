package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	managementStatePath       = "/v0/management/plugins/lzz-groups/state"
	managementRefreshPath     = "/v0/management/plugins/lzz-groups/refresh"
	managementModelsPath      = "/v0/management/plugins/lzz-groups/models"
	managementQuotaHealthPath = "/v0/management/plugins/lzz-groups/quota-health"
	managementUsagePath       = "/v0/management/plugins/lzz-groups/usage"
	resourceUIPath            = "/v0/resource/plugins/lzz-groups/ui"
)

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes"`
	Resources []managementResource `json:"resources"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type authView struct {
	ID                   string   `json:"id"`
	AuthIndex            string   `json:"auth_index"`
	Name                 string   `json:"name"`
	Provider             string   `json:"provider"`
	Label                string   `json:"label"`
	Status               string   `json:"status"`
	StatusMessage        string   `json:"status_message,omitempty"`
	Disabled             bool     `json:"disabled"`
	Unavailable          bool     `json:"unavailable"`
	RuntimeOnly          bool     `json:"runtime_only"`
	Source               string   `json:"source,omitempty"`
	AccountType          string   `json:"account_type,omitempty"`
	Account              string   `json:"account,omitempty"`
	Priority             int      `json:"priority"`
	Groups               []string `json:"groups"`
	GroupSource          string   `json:"group_source,omitempty"`
	SupportedModels      []string `json:"supported_models,omitempty"`
	SupportedModelsCount int      `json:"supported_models_count"`
}

type pluginStateResponse struct {
	Success          bool         `json:"success"`
	Version          string       `json:"version"`
	LastRefresh      time.Time    `json:"last_refresh,omitempty"`
	LastRefreshError string       `json:"last_refresh_error,omitempty"`
	Auths            []authView   `json:"auths"`
	Policies         []policyView `json:"policies"`
}

type policyView struct {
	ID         string   `json:"id"`
	Label      string   `json:"label,omitempty"`
	KeyPreview string   `json:"key_preview"`
	Groups     []string `json:"groups"`
}

type modelsRequest struct {
	APIKey string `json:"api_key"`
}

type modelView struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type quotaHealthRequest struct {
	APIKey string   `json:"api_key"`
	Models []string `json:"models"`
}

type quotaHealthResponse struct {
	Success                bool     `json:"success"`
	QuotaAvailable         bool     `json:"quota_available"`
	Reason                 string   `json:"reason,omitempty"`
	NextAvailableAt        int64    `json:"next_available_at,omitempty"`
	RetryAfterSeconds      int64    `json:"retry_after_seconds,omitempty"`
	Groups                 []string `json:"groups,omitempty"`
	Models                 []string `json:"models,omitempty"`
	TotalCandidates        int      `json:"total_candidates"`
	AvailableCandidates    int      `json:"available_candidates"`
	QuotaBlockedCandidates int      `json:"quota_blocked_candidates"`
	OtherBlockedCandidates int      `json:"other_blocked_candidates"`
}

func registerManagementRoutes() []byte {
	return okEnvelope(managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: "/plugins/lzz-groups/state", Description: "Returns group policies and credential state."},
			{Method: http.MethodPost, Path: "/plugins/lzz-groups/refresh", Description: "Refreshes credential metadata from the host."},
			{Method: http.MethodPost, Path: "/plugins/lzz-groups/models", Description: "Returns models visible to one downstream API key."},
			{Method: http.MethodPost, Path: "/plugins/lzz-groups/quota-health", Description: "Returns group-scoped quota health."},
			{Method: http.MethodPost, Path: "/plugins/lzz-groups/usage", Description: "Returns official Codex quota windows for one downstream API key."},
		},
		Resources: []managementResource{{
			Path:        "/ui",
			Menu:        "分组管理",
			Description: "管理下游密钥策略和账号分组",
		}},
	})
}

func handleManagementRequest(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	if req.Path == resourceUIPath {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       []byte(groupManagementHTML),
		}), nil
	}

	var status int
	var response any
	switch {
	case req.Method == http.MethodGet && req.Path == managementStatePath:
		_ = refreshAuthCache(false)
		status = http.StatusOK
		response = buildPluginStateResponse()
	case req.Method == http.MethodPost && req.Path == managementRefreshPath:
		if errRefresh := refreshAuthCache(true); errRefresh != nil {
			status = http.StatusBadGateway
			response = map[string]any{"success": false, "error": errRefresh.Error()}
		} else {
			status = http.StatusOK
			response = buildPluginStateResponse()
		}
	case req.Method == http.MethodPost && req.Path == managementModelsPath:
		var body modelsRequest
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			status = http.StatusBadRequest
			response = map[string]any{"success": false, "error": "invalid_request_body"}
		} else {
			status, response = modelsForAPIKey(body.APIKey)
		}
	case req.Method == http.MethodPost && req.Path == managementQuotaHealthPath:
		var body quotaHealthRequest
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			status = http.StatusBadRequest
			response = map[string]any{"success": false, "error": "invalid_request_body"}
		} else {
			status, response = quotaHealthForAPIKey(body)
		}
	case req.Method == http.MethodPost && req.Path == managementUsagePath:
		var body usageRequest
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			status = http.StatusBadRequest
			response = map[string]any{"success": false, "error": "invalid_request_body"}
		} else {
			status, response = usageForAPIKey(body)
		}
	default:
		status = http.StatusNotFound
		response = map[string]any{"success": false, "error": "route_not_found"}
	}
	body, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}), nil
}

func buildPluginStateResponse() pluginStateResponse {
	auths := cachedAuthSnapshot()
	views := make([]authView, 0, len(auths))
	for _, auth := range auths {
		entry := auth.Entry
		views = append(views, authView{
			ID:                   entry.ID,
			AuthIndex:            entry.AuthIndex,
			Name:                 entry.Name,
			Provider:             firstNonEmpty(entry.Provider, entry.Type),
			Label:                firstNonEmpty(entry.Label, entry.Email, entry.Name),
			Status:               entry.Status,
			StatusMessage:        entry.StatusMessage,
			Disabled:             entry.Disabled,
			Unavailable:          entry.Unavailable,
			RuntimeOnly:          entry.RuntimeOnly,
			Source:               entry.Source,
			AccountType:          entry.AccountType,
			Account:              entry.Account,
			Priority:             entry.Priority,
			Groups:               append([]string(nil), auth.Groups...),
			GroupSource:          auth.GroupSource,
			SupportedModels:      append([]string(nil), auth.SupportedModels...),
			SupportedModelsCount: len(auth.SupportedModels),
		})
	}

	pluginRuntime.mu.RLock()
	policies := make([]policyView, 0, len(pluginRuntime.policiesByKey))
	for _, policy := range pluginRuntime.policiesByKey {
		policies = append(policies, policyView{
			ID:         policy.ID,
			Label:      policy.Label,
			KeyPreview: maskAPIKey(policy.Key),
			Groups:     append([]string(nil), policy.Groups...),
		})
	}
	lastRefresh := pluginRuntime.lastRefresh
	lastRefreshError := pluginRuntime.lastRefreshError
	pluginRuntime.mu.RUnlock()
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return pluginStateResponse{
		Success:          true,
		Version:          pluginVersion,
		LastRefresh:      lastRefresh,
		LastRefreshError: lastRefreshError,
		Auths:            views,
		Policies:         policies,
	}
}

func modelsForAPIKey(rawKey string) (int, any) {
	policy, ok := policyForAPIKey(rawKey)
	if !ok {
		return http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"}
	}
	_ = refreshAuthCache(false)
	type modelOwner struct {
		provider string
	}
	models := make(map[string]modelOwner)
	for _, auth := range cachedAuthSnapshot() {
		if auth.Entry.Disabled || !policyAllowsAuth(policy, auth.Groups) {
			continue
		}
		provider := firstNonEmpty(auth.Entry.Provider, auth.Entry.Type, "unknown")
		for _, modelID := range auth.SupportedModels {
			models[modelID] = modelOwner{provider: provider}
		}
	}
	ids := make([]string, 0, len(models))
	for modelID := range models {
		ids = append(ids, modelID)
	}
	sort.Strings(ids)
	data := make([]modelView, 0, len(ids))
	for _, modelID := range ids {
		data = append(data, modelView{ID: modelID, Object: "model", OwnedBy: models[modelID].provider})
	}
	return http.StatusOK, map[string]any{
		"success": true,
		"object":  "list",
		"data":    data,
		"models":  ids,
		"groups":  policy.Groups,
	}
}

func quotaHealthForAPIKey(req quotaHealthRequest) (int, any) {
	policy, ok := policyForAPIKey(req.APIKey)
	if !ok {
		return http.StatusUnauthorized, quotaHealthResponse{Success: false, Reason: "unauthorized"}
	}
	_ = refreshAuthCache(false)
	result := quotaHealthResponse{
		Success: true,
		Groups:  append([]string(nil), policy.Groups...),
		Models:  normalizeModels(req.Models),
	}
	now := time.Now()
	earliest := time.Time{}
	for _, auth := range cachedAuthSnapshot() {
		if auth.Entry.Disabled || !policyAllowsAuth(policy, auth.Groups) {
			continue
		}
		models := result.Models
		if len(models) == 0 {
			models = []string{""}
		}
		for _, modelID := range models {
			if modelID != "" && !containsString(auth.SupportedModels, modelID) {
				continue
			}
			result.TotalCandidates++
			available, quotaBlocked, recoverAt := classifyAuthAvailability(auth, modelID)
			switch {
			case available:
				result.AvailableCandidates++
			case quotaBlocked:
				result.QuotaBlockedCandidates++
				if !recoverAt.IsZero() && recoverAt.After(now) && (earliest.IsZero() || recoverAt.Before(earliest)) {
					earliest = recoverAt
				}
			default:
				result.OtherBlockedCandidates++
			}
		}
	}

	switch {
	case result.TotalCandidates == 0:
		result.QuotaAvailable = true
		result.Reason = "no_matching_auth"
	case result.AvailableCandidates > 0:
		result.QuotaAvailable = true
		result.Reason = "quota_available"
	case result.OtherBlockedCandidates > 0:
		result.QuotaAvailable = true
		result.Reason = "blocked_by_non_quota_state"
	case result.QuotaBlockedCandidates > 0:
		result.QuotaAvailable = false
		result.Reason = "quota_exhausted"
		if !earliest.IsZero() {
			result.NextAvailableAt = earliest.Unix()
			seconds := int64(earliest.Sub(now).Seconds())
			if earliest.After(now.Add(time.Duration(seconds) * time.Second)) {
				seconds++
			}
			if seconds > 0 {
				result.RetryAfterSeconds = seconds
			}
		}
	default:
		result.QuotaAvailable = true
		result.Reason = "quota_available"
	}
	return http.StatusOK, result
}

func classifyAuthAvailability(auth cachedAuth, modelID string) (available bool, quotaBlocked bool, recoverAt time.Time) {
	quota := auth.Quota
	unavailable := auth.Entry.Unavailable || (!strings.EqualFold(auth.Entry.Status, "active") && auth.Entry.Status != "")
	retryAt := auth.Entry.NextRetryAfter
	if state, ok := auth.ModelStates[modelID]; ok && modelID != "" {
		quota = state.Quota
		unavailable = state.Unavailable || (!strings.EqualFold(state.Status, "active") && state.Status != "")
		retryAt = state.NextRetryAfter
	}
	if quota.Exceeded {
		recoverAt = quota.NextRecoverAt
		if recoverAt.IsZero() {
			recoverAt = retryAt
		}
		return false, true, recoverAt
	}
	if unavailable {
		return false, false, time.Time{}
	}
	return true, false, time.Time{}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func maskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 8 {
		return "********"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
