package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type schedulerFilterResponse struct {
	EligibleAuthIDs []string `json:"EligibleAuthIDs"`
	DelegateBuiltin string   `json:"DelegateBuiltin"`
	Handled         bool     `json:"Handled"`
}

func authenticateFrontend(raw []byte) ([]byte, error) {
	var req pluginapi.FrontendAuthRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode frontend auth request: %w", errUnmarshal)
	}
	for _, candidate := range frontendCredentialCandidates(req.Headers, req.Query) {
		if policy, ok := policyForAPIKey(candidate); ok {
			return okEnvelope(pluginapi.FrontendAuthResponse{
				Authenticated: true,
				Principal:     policy.Principal,
				Metadata: map[string]string{
					"policy_id": policy.ID,
					"label":     policy.Label,
				},
			}), nil
		}
	}
	return okEnvelope(pluginapi.FrontendAuthResponse{Authenticated: false}), nil
}

func frontendCredentialCandidates(headers http.Header, query url.Values) []string {
	result := make([]string, 0, 5)
	if authorization := strings.TrimSpace(headers.Get("Authorization")); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			result = append(result, strings.TrimSpace(parts[1]))
		} else {
			result = append(result, authorization)
		}
	}
	result = append(result,
		strings.TrimSpace(headers.Get("X-Goog-Api-Key")),
		strings.TrimSpace(headers.Get("X-Api-Key")),
		strings.TrimSpace(query.Get("key")),
		strings.TrimSpace(query.Get("auth_token")),
	)
	return result
}

func pickSchedulerCandidates(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode scheduler request: %w", errUnmarshal)
	}
	callerScopeValue, _ := req.Options.Metadata["caller_scope"].(string)
	policy, ok := policyForCallerScope(callerScopeValue)
	if !ok {
		return errorEnvelope("group_policy_missing", "authenticated caller has no group policy", http.StatusForbidden), nil
	}

	pluginRuntime.mu.RLock()
	eligible := make([]string, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		auth, exists := pluginRuntime.authsByID[candidate.ID]
		if !exists {
			if len(policy.Groups) == 0 || containsWildcard(policy.Groups) {
				eligible = append(eligible, candidate.ID)
			}
			continue
		}
		if policyAllowsAuth(policy, auth.Groups) {
			eligible = append(eligible, candidate.ID)
		}
	}
	pluginRuntime.mu.RUnlock()

	return okEnvelope(schedulerFilterResponse{
		EligibleAuthIDs: eligible,
		DelegateBuiltin: "configured",
		Handled:         true,
	}), nil
}
