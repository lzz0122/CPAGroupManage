package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostQuotaState struct {
	Exceeded      bool      `json:"exceeded"`
	Reason        string    `json:"reason"`
	NextRecoverAt time.Time `json:"next_recover_at"`
	ObservedAt    time.Time `json:"observed_at"`
}

type hostModelState struct {
	Status         string         `json:"status"`
	StatusMessage  string         `json:"status_message"`
	Unavailable    bool           `json:"unavailable"`
	NextRetryAfter time.Time      `json:"next_retry_after"`
	Quota          hostQuotaState `json:"quota"`
}

type hostAuthRuntimeResponse struct {
	Auth            pluginapi.HostAuthFileEntry `json:"auth"`
	SupportedModels []string                    `json:"supported_models"`
	Quota           hostQuotaState              `json:"quota"`
	ModelStates     map[string]hostModelState   `json:"model_states"`
}

const hostFeaturesMethod = "host.features"

var requiredHostFeatures = []string{
	"scheduler.eligible-auth-filter.v1",
	"auth.runtime-inspection.v1",
}

type hostFeaturesResponse struct {
	Features []string `json:"features"`
}

func requireHostFeatures() error {
	var response hostFeaturesResponse
	if errCall := callHost(hostFeaturesMethod, map[string]any{}, &response); errCall != nil {
		return fmt.Errorf("CPA host does not expose the required plugin extensions: %w", errCall)
	}
	available := make(map[string]struct{}, len(response.Features))
	for _, feature := range response.Features {
		if feature = strings.TrimSpace(feature); feature != "" {
			available[feature] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, feature := range requiredHostFeatures {
		if _, ok := available[feature]; !ok {
			missing = append(missing, feature)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("CPA host is missing required plugin features: %s", strings.Join(missing, ", "))
	}
	return nil
}

func callHost(method string, request any, response any) error {
	rawRequest, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return fmt.Errorf("encode %s request: %w", method, errMarshal)
	}
	rawResponse, errCall := callHostRaw(method, rawRequest)
	if errCall != nil {
		return errCall
	}
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
		return fmt.Errorf("decode %s envelope: %w", method, errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return fmt.Errorf("%s: %s", envelope.Error.Code, envelope.Error.Message)
		}
		return fmt.Errorf("host callback %s failed", method)
	}
	if response == nil {
		return nil
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, response); errUnmarshal != nil {
		return fmt.Errorf("decode %s result: %w", method, errUnmarshal)
	}
	return nil
}

func refreshAuthCache(force bool) error {
	pluginRuntime.refreshMu.Lock()
	defer pluginRuntime.refreshMu.Unlock()

	pluginRuntime.mu.RLock()
	lastRefresh := pluginRuntime.lastRefresh
	pluginRuntime.mu.RUnlock()
	if !force && !lastRefresh.IsZero() && time.Since(lastRefresh) < 5*time.Second {
		return nil
	}

	var list hostAuthListResponse
	if errList := callHost(pluginabi.MethodHostAuthList, map[string]any{}, &list); errList != nil {
		pluginRuntime.mu.Lock()
		pluginRuntime.lastRefreshError = errList.Error()
		pluginRuntime.mu.Unlock()
		return errList
	}

	next := make(map[string]cachedAuth, len(list.Files))
	for _, entry := range list.Files {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		cached := cachedAuth{Entry: entry}
		var runtimeResp hostAuthRuntimeResponse
		if entry.AuthIndex != "" {
			if errRuntime := callHost(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex}, &runtimeResp); errRuntime == nil {
				cached.Entry = runtimeResp.Auth
				cached.SupportedModels = normalizeModels(runtimeResp.SupportedModels)
				cached.Quota = runtimeResp.Quota
				cached.ModelStates = runtimeResp.ModelStates
			}
		}

		if groups, configured := configuredCredentialGroups(entry.ID, entry.AuthIndex); configured {
			cached.Groups = groups
			cached.GroupSource = "plugin-config"
		} else if entry.AuthIndex != "" && strings.EqualFold(entry.Source, "file") {
			var physical pluginapi.HostAuthGetResponse
			if errGet := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex}, &physical); errGet == nil {
				cached.Groups = groupsFromAuthJSON(physical.JSON)
				if len(cached.Groups) > 0 {
					cached.GroupSource = "auth-file"
				}
			}
		}
		next[entry.ID] = cached
	}

	pluginRuntime.mu.Lock()
	pluginRuntime.authsByID = next
	pluginRuntime.lastRefresh = time.Now().UTC()
	pluginRuntime.lastRefreshError = ""
	pluginRuntime.mu.Unlock()
	return nil
}

func groupsFromAuthJSON(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil
	}
	groups := make([]string, 0)
	for _, key := range []string{"groups", "group"} {
		switch value := payload[key].(type) {
		case string:
			groups = append(groups, value)
		case []any:
			for _, item := range value {
				if text, ok := item.(string); ok {
					groups = append(groups, text)
				}
			}
		}
	}
	return normalizeGroups(groups)
}

func normalizeModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	sort.Strings(result)
	return result
}
