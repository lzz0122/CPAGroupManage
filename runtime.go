package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginID      = "lzz-groups"
	pluginName    = "LZZ Groups"
	pluginVersion = "0.1.0"
)

type apiKeyPolicy struct {
	ID     string   `yaml:"id" json:"id"`
	Label  string   `yaml:"label,omitempty" json:"label,omitempty"`
	Key    string   `yaml:"key" json:"key"`
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"`
}

type pluginConfig struct {
	APIKeys          []apiKeyPolicy      `yaml:"api-keys" json:"api-keys"`
	CredentialGroups map[string][]string `yaml:"credential-groups" json:"credential-groups"`
	RefreshSeconds   int                 `yaml:"refresh-seconds" json:"refresh-seconds"`
}

type runtimePolicy struct {
	ID          string
	Label       string
	Key         string
	Principal   string
	CallerScope string
	Groups      []string
}

type cachedAuth struct {
	Entry           pluginapi.HostAuthFileEntry
	Groups          []string
	GroupSource     string
	SupportedModels []string
	Quota           hostQuotaState
	ModelStates     map[string]hostModelState
}

type runtimeState struct {
	mu               sync.RWMutex
	refreshMu        sync.Mutex
	config           pluginConfig
	policiesByKey    map[string]runtimePolicy
	policiesByCaller map[string]runtimePolicy
	authsByID        map[string]cachedAuth
	lastRefresh      time.Time
	lastRefreshError string
	stop             chan struct{}
	done             chan struct{}
}

var pluginRuntime = runtimeState{
	policiesByKey:    make(map[string]runtimePolicy),
	policiesByCaller: make(map[string]runtimePolicy),
	authsByID:        make(map[string]cachedAuth),
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive"`
	Scheduler                     bool `json:"scheduler"`
	ManagementAPI                 bool `json:"management_api"`
}

func handlePluginMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errCompatibility := requireHostFeatures(); errCompatibility != nil {
			return errorEnvelope("incompatible_host", errCompatibility.Error(), 0), nil
		}
		var req lifecycleRequest
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
		if errApply := applyPluginConfig(req.ConfigYAML); errApply != nil {
			return errorEnvelope("invalid_config", errApply.Error(), 0), nil
		}
		startRefreshLoop()
		go func() { _ = refreshAuthCache(true) }()
		return okEnvelope(pluginRegistration()), nil
	case pluginabi.MethodPluginQuiesce:
		stopRefreshLoop()
		return okEnvelope(struct{}{}), nil
	case pluginabi.MethodFrontendAuthIdentifier:
		return okEnvelope(struct {
			Identifier string `json:"identifier"`
		}{Identifier: pluginID}), nil
	case pluginabi.MethodFrontendAuthAuthenticate:
		return authenticateFrontend(raw)
	case pluginabi.MethodSchedulerPick:
		return pickSchedulerCandidates(raw)
	case pluginabi.MethodManagementRegister:
		return registerManagementRoutes(), nil
	case pluginabi.MethodManagementHandle:
		return handleManagementRequest(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "lzz122",
			GitHubRepository: "https://github.com/lzz122/cpa-plugin-lzz-groups",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "api-keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Downstream API keys and allowed credential groups."},
				{Name: "credential-groups", Type: pluginapi.ConfigFieldTypeObject, Description: "Credential group assignments keyed by auth_index or auth ID."},
				{Name: "refresh-seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Credential metadata refresh interval (5-3600 seconds)."},
			},
		},
		Capabilities: registrationCapabilities{
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: true,
			Scheduler:                     true,
			ManagementAPI:                 true,
		},
	}
}

func applyPluginConfig(raw []byte) error {
	var cfg pluginConfig
	if len(strings.TrimSpace(string(raw))) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
			return fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	if cfg.RefreshSeconds == 0 {
		cfg.RefreshSeconds = 60
	}
	if cfg.RefreshSeconds < 5 || cfg.RefreshSeconds > 3600 {
		return errors.New("refresh-seconds must be between 5 and 3600")
	}

	byKey := make(map[string]runtimePolicy, len(cfg.APIKeys))
	byCaller := make(map[string]runtimePolicy, len(cfg.APIKeys))
	for index := range cfg.APIKeys {
		policy := &cfg.APIKeys[index]
		policy.Key = strings.TrimSpace(policy.Key)
		if policy.Key == "" {
			return fmt.Errorf("api-keys[%d].key is required", index)
		}
		policy.ID = strings.TrimSpace(policy.ID)
		if policy.ID == "" {
			policy.ID = "key-" + shortHash(policy.Key)
		}
		policy.Label = strings.TrimSpace(policy.Label)
		policy.Groups = normalizeGroups(policy.Groups)
		if _, exists := byKey[policy.Key]; exists {
			return fmt.Errorf("duplicate API key at api-keys[%d]", index)
		}
		principal := pluginID + ":" + policy.ID
		runtimePolicy := runtimePolicy{
			ID:          policy.ID,
			Label:       policy.Label,
			Key:         policy.Key,
			Principal:   principal,
			CallerScope: callerScope(principal),
			Groups:      append([]string(nil), policy.Groups...),
		}
		if _, exists := byCaller[runtimePolicy.CallerScope]; exists {
			return fmt.Errorf("duplicate API key policy id %q", policy.ID)
		}
		byKey[policy.Key] = runtimePolicy
		byCaller[runtimePolicy.CallerScope] = runtimePolicy
	}

	if cfg.CredentialGroups == nil {
		cfg.CredentialGroups = make(map[string][]string)
	}
	normalizedAssignments := make(map[string][]string, len(cfg.CredentialGroups))
	for identity, groups := range cfg.CredentialGroups {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			continue
		}
		normalizedAssignments[identity] = normalizeGroups(groups)
	}
	cfg.CredentialGroups = normalizedAssignments

	stopRefreshLoop()
	pluginRuntime.refreshMu.Lock()
	defer pluginRuntime.refreshMu.Unlock()
	pluginRuntime.mu.Lock()
	pluginRuntime.config = cfg
	pluginRuntime.policiesByKey = byKey
	pluginRuntime.policiesByCaller = byCaller
	pluginRuntime.authsByID = make(map[string]cachedAuth)
	pluginRuntime.lastRefresh = time.Time{}
	pluginRuntime.lastRefreshError = ""
	pluginRuntime.mu.Unlock()
	return nil
}

func startRefreshLoop() {
	pluginRuntime.mu.Lock()
	if pluginRuntime.stop != nil {
		pluginRuntime.mu.Unlock()
		return
	}
	interval := time.Duration(pluginRuntime.config.RefreshSeconds) * time.Second
	stop := make(chan struct{})
	done := make(chan struct{})
	pluginRuntime.stop = stop
	pluginRuntime.done = done
	pluginRuntime.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = refreshAuthCache(false)
			}
		}
	}()
}

func stopRefreshLoop() {
	pluginRuntime.mu.Lock()
	stop := pluginRuntime.stop
	done := pluginRuntime.done
	pluginRuntime.stop = nil
	pluginRuntime.done = nil
	pluginRuntime.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	if done != nil {
		<-done
	}
}

func configuredCredentialGroups(identity, authIndex string) ([]string, bool) {
	pluginRuntime.mu.RLock()
	defer pluginRuntime.mu.RUnlock()
	for _, key := range []string{strings.TrimSpace(authIndex), strings.TrimSpace(identity)} {
		if key == "" {
			continue
		}
		groups, ok := pluginRuntime.config.CredentialGroups[key]
		if ok {
			return append([]string(nil), groups...), true
		}
	}
	return nil, false
}

func policyForAPIKey(key string) (runtimePolicy, bool) {
	pluginRuntime.mu.RLock()
	defer pluginRuntime.mu.RUnlock()
	policy, ok := pluginRuntime.policiesByKey[strings.TrimSpace(key)]
	return policy, ok
}

func policyForCallerScope(scope string) (runtimePolicy, bool) {
	pluginRuntime.mu.RLock()
	defer pluginRuntime.mu.RUnlock()
	policy, ok := pluginRuntime.policiesByCaller[strings.TrimSpace(scope)]
	return policy, ok
}

func cachedAuthSnapshot() []cachedAuth {
	pluginRuntime.mu.RLock()
	defer pluginRuntime.mu.RUnlock()
	result := make([]cachedAuth, 0, len(pluginRuntime.authsByID))
	for _, auth := range pluginRuntime.authsByID {
		copyAuth := auth
		copyAuth.Groups = append([]string(nil), auth.Groups...)
		copyAuth.SupportedModels = append([]string(nil), auth.SupportedModels...)
		result = append(result, copyAuth)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Entry.Name) < strings.ToLower(result[j].Entry.Name)
	})
	return result
}

func normalizeGroups(groups []string) []string {
	seen := make(map[string]struct{}, len(groups))
	result := make([]string, 0, len(groups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		key := strings.ToLower(group)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, group)
	}
	return result
}

func groupsIntersect(left, right []string) bool {
	if containsWildcard(left) || containsWildcard(right) {
		return true
	}
	seen := make(map[string]struct{}, len(left))
	for _, value := range normalizeGroups(left) {
		seen[strings.ToLower(value)] = struct{}{}
	}
	for _, value := range normalizeGroups(right) {
		if _, ok := seen[strings.ToLower(value)]; ok {
			return true
		}
	}
	return false
}

func containsWildcard(groups []string) bool {
	for _, group := range groups {
		if strings.TrimSpace(group) == "*" {
			return true
		}
	}
	return false
}

func policyAllowsAuth(policy runtimePolicy, groups []string) bool {
	if len(policy.Groups) == 0 || containsWildcard(policy.Groups) {
		return true
	}
	return groupsIntersect(policy.Groups, groups)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func callerScope(principal string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(principal)))
	return hex.EncodeToString(sum[:])
}
