package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var projectMutationMu sync.Mutex

const (
	maxProjectManagementBodyBytes = 256 << 10
	projectStatusActive           = "active"
	projectStatusDisabled         = "disabled"
	projectStatusRevoked          = "revoked"
)

type projectView struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Enabled        bool                   `json:"enabled"`
	Status         string                 `json:"status"`
	Models         []string               `json:"models"`
	PrimaryAuthIDs []string               `json:"primary_auth_ids"`
	AllowedAuthIDs []string               `json:"allowed_auth_ids"`
	Policy         map[string]any         `json:"policy"`
	PromptCache    projectPromptCacheView `json:"prompt_cache"`
	CreatedAt      string                 `json:"created_at,omitempty"`
	UpdatedAt      string                 `json:"updated_at,omitempty"`
	Usage          usageSummaryView       `json:"usage"`
}

type projectModelOption struct {
	ID           string `json:"id"`
	RequestModel string `json:"request_model"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description,omitempty"`
}

type createProjectRequest struct {
	Name           string                    `json:"name"`
	Enabled        *bool                     `json:"enabled,omitempty"`
	Status         string                    `json:"status,omitempty"`
	Models         []string                  `json:"models,omitempty"`
	PrimaryAuthIDs []string                  `json:"primary_auth_ids,omitempty"`
	AllowedAuthIDs []string                  `json:"allowed_auth_ids,omitempty"`
	Policy         map[string]any            `json:"policy,omitempty"`
	PromptCache    *projectPromptCachePolicy `json:"prompt_cache,omitempty"`
}

type patchProjectRequest struct {
	ID             string                    `json:"id"`
	Name           *string                   `json:"name,omitempty"`
	Enabled        *bool                     `json:"enabled,omitempty"`
	Status         *string                   `json:"status,omitempty"`
	Models         *[]string                 `json:"models,omitempty"`
	PrimaryAuthIDs *[]string                 `json:"primary_auth_ids,omitempty"`
	AllowedAuthIDs *[]string                 `json:"allowed_auth_ids,omitempty"`
	Policy         json.RawMessage           `json:"policy,omitempty"`
	PromptCache    *projectPromptCachePolicy `json:"prompt_cache,omitempty"`
}

type projectIdentityRequest struct {
	ID string `json:"id"`
}

type hostPluginConfigListMutationRequest struct {
	HostCallbackID string          `json:"host_callback_id"`
	Field          string          `json:"field"`
	Operation      string          `json:"operation"`
	MatchField     string          `json:"match_field"`
	MatchValue     string          `json:"match_value"`
	Value          json.RawMessage `json:"value,omitempty"`
	UniqueFields   []string        `json:"unique_fields,omitempty"`
}

type hostPluginConfigListMutationResult struct {
	Items []json.RawMessage `json:"items"`
}

func handleProjectsManagement(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if (path == "/v0/management/bravo/projects" && req.Method != http.MethodGet) ||
		path == "/v0/management/bravo/projects/rotate" {
		projectMutationMu.Lock()
		defer projectMutationMu.Unlock()
	}
	switch {
	case path == "/v0/management/bravo/projects" && req.Method == http.MethodGet:
		return listProjects(req)
	case path == "/v0/management/bravo/projects" && req.Method == http.MethodPost:
		return createProject(req)
	case path == "/v0/management/bravo/projects" && req.Method == http.MethodPatch:
		return patchProject(req)
	case path == "/v0/management/bravo/projects" && req.Method == http.MethodDelete:
		return deleteProject(req)
	case path == "/v0/management/bravo/projects/rotate" && req.Method == http.MethodPost:
		return rotateProject(req)
	default:
		return nil, nil
	}
}

func listProjects(req rpcManagementRequest) ([]byte, error) {
	cfg := loadedConfig()
	projects := make([]projectView, 0, len(cfg.SmartKeys))
	for _, item := range cfg.SmartKeys {
		projects = append(projects, smartKeyProjectView(item))
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name == projects[j].Name {
			return projects[i].ID < projects[j].ID
		}
		return projects[i].Name < projects[j].Name
	})

	modelNames := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	models := make([]projectModelOption, 0, len(modelNames))
	for _, name := range modelNames {
		model := cfg.Models[name]
		models = append(models, projectModelOption{
			ID:           name,
			RequestModel: cfg.Prefix + name,
			DisplayName:  model.DisplayName,
			Description:  model.Description,
		})
	}
	return managementJSON(http.StatusOK, map[string]any{
		"projects": projects,
		"models":   models,
		"tariffs":  append([]tariffConfig(nil), cfg.Tariffs...),
	})
}

func createProject(req rpcManagementRequest) ([]byte, error) {
	var input createProjectRequest
	if failure := decodeProjectManagementBody(req.Body, &input); failure != nil {
		return projectFailureJSON(*failure)
	}
	cfg := loadedConfig()
	name, failure := normalizeProjectName(input.Name)
	if failure != nil {
		return projectFailureJSON(*failure)
	}
	if _, exists := findProjectByName(cfg, name); exists {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_project_name_exists",
			Message: "A Bravo project with this name already exists.",
			Status:  http.StatusConflict,
		})
	}
	models, failure := normalizeProjectModels(cfg, input.Models, true)
	if failure != nil {
		return projectFailureJSON(*failure)
	}
	enabled, status, failure := normalizeProjectState(input.Enabled, input.Status, true)
	if failure != nil {
		return projectFailureJSON(*failure)
	}
	if status == projectStatusRevoked {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_project_state_invalid",
			Message: "A new project cannot start in the revoked state.",
			Status:  http.StatusBadRequest,
		})
	}
	projectID, errID := generateUniqueProjectID(cfg)
	if errID != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_project_id_generation_failed",
			Message: "Could not generate a project identifier.",
			Status:  http.StatusInternalServerError,
		})
	}
	plaintext, digest, errKey := generateProjectKey()
	if errKey != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_project_key_generation_failed",
			Message: "Could not generate a project key.",
			Status:  http.StatusInternalServerError,
		})
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	policy := cloneProjectPolicy(input.Policy)
	if input.PromptCache != nil {
		if failure := setProjectPromptCachePolicy(policy, *input.PromptCache); failure != nil {
			return projectFailureJSON(*failure)
		}
	}
	if failure := validateAndCanonicalizeProjectPromptCachePolicy(policy); failure != nil {
		return projectFailureJSON(*failure)
	}
	item := smartKeyConfig{
		ID:             projectID,
		Name:           name,
		SHA256:         digest,
		Enabled:        boolPointer(enabled),
		Status:         status,
		Models:         models,
		PrimaryAuthIDs: normalizeOpaqueStrings(input.PrimaryAuthIDs),
		AllowedAuthIDs: normalizeOpaqueStrings(input.AllowedAuthIDs),
		Policy:         policy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if failure := validateAndCanonicalizeProjectPrimaries(req.HostCallbackID, cfg, nil, &item); failure != nil {
		return projectFailureJSON(*failure)
	}
	items, errPersist := persistSmartKeyMutation(
		req.HostCallbackID,
		"append",
		"id",
		item.ID,
		&item,
	)
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedSmartKeys(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return managementJSON(http.StatusCreated, map[string]any{
		"project":       smartKeyProjectView(item),
		"plaintext_key": plaintext,
		"project_api": map[string]any{
			"limits": projectLimitsDocs(),
			"routes": projectRoutesDocs(),
		},
	})
}

func patchProject(req rpcManagementRequest) ([]byte, error) {
	var input patchProjectRequest
	if failure := decodeProjectManagementBody(req.Body, &input); failure != nil {
		return projectFailureJSON(*failure)
	}
	cfg := loadedConfig()
	current, exists := findProjectByID(cfg, input.ID)
	if !exists {
		return projectNotFoundJSON()
	}
	updated := cloneSmartKeyConfig(current)
	if input.Name != nil {
		name, failure := normalizeProjectName(*input.Name)
		if failure != nil {
			return projectFailureJSON(*failure)
		}
		if other, duplicate := findProjectByName(cfg, name); duplicate && other.ID != current.ID {
			return projectFailureJSON(projectFailure{
				Code:    "bravo_project_name_exists",
				Message: "A Bravo project with this name already exists.",
				Status:  http.StatusConflict,
			})
		}
		updated.Name = name
	}
	if input.Models != nil {
		models, failure := normalizeProjectModels(cfg, *input.Models, false)
		if failure != nil {
			return projectFailureJSON(*failure)
		}
		updated.Models = models
	}
	if input.PrimaryAuthIDs != nil {
		updated.PrimaryAuthIDs = normalizeOpaqueStrings(*input.PrimaryAuthIDs)
	}
	if input.AllowedAuthIDs != nil {
		updated.AllowedAuthIDs = normalizeOpaqueStrings(*input.AllowedAuthIDs)
	}
	if input.Policy != nil {
		if strings.TrimSpace(string(input.Policy)) == "null" {
			updated.Policy = map[string]any{}
		} else {
			var policy map[string]any
			if errPolicy := json.Unmarshal(input.Policy, &policy); errPolicy != nil || policy == nil {
				return projectFailureJSON(projectFailure{
					Code:    "bravo_project_policy_invalid",
					Message: "policy must be an object or null.",
					Status:  http.StatusBadRequest,
				})
			}
			updated.Policy = cloneProjectPolicy(policy)
		}
	}
	if input.PromptCache != nil {
		if failure := setProjectPromptCachePolicy(updated.Policy, *input.PromptCache); failure != nil {
			return projectFailureJSON(*failure)
		}
	}
	if failure := validateAndCanonicalizeProjectPromptCachePolicy(updated.Policy); failure != nil {
		return projectFailureJSON(*failure)
	}
	if strings.EqualFold(current.Status, projectStatusRevoked) {
		requestedStatus := projectStatusRevoked
		if input.Status != nil {
			requestedStatus = strings.ToLower(strings.TrimSpace(*input.Status))
		}
		if (input.Enabled != nil && *input.Enabled) || requestedStatus != projectStatusRevoked {
			return projectFailureJSON(projectFailure{
				Code:    "bravo_project_revoked",
				Message: "A revoked project cannot be re-enabled. Create or rotate a new active project instead.",
				Status:  http.StatusConflict,
			})
		}
		updated.Enabled = boolPointer(false)
		updated.Status = projectStatusRevoked
	} else if input.Enabled != nil || input.Status != nil {
		statusInput := ""
		if input.Status != nil {
			statusInput = *input.Status
		}
		enabled, status, failure := normalizeProjectState(input.Enabled, statusInput, smartKeyEnabled(updated))
		if failure != nil {
			return projectFailureJSON(*failure)
		}
		updated.Enabled = boolPointer(enabled)
		updated.Status = status
	}
	updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if failure := validateAndCanonicalizeProjectPrimaries(req.HostCallbackID, cfg, &current, &updated); failure != nil {
		return projectFailureJSON(*failure)
	}
	items, errPersist := persistSmartKeyMutation(
		req.HostCallbackID,
		"replace",
		smartKeyMatchField(current),
		smartKeyMatchValue(current),
		&updated,
	)
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedSmartKeys(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return managementJSON(http.StatusOK, map[string]any{
		"project": smartKeyProjectView(updated),
	})
}

func deleteProject(req rpcManagementRequest) ([]byte, error) {
	projectID := strings.TrimSpace(firstQueryValue(req.Query, "id"))
	if projectID == "" && len(req.Body) > 0 {
		var input projectIdentityRequest
		if failure := decodeProjectManagementBody(req.Body, &input); failure != nil {
			return projectFailureJSON(*failure)
		}
		projectID = strings.TrimSpace(input.ID)
	}
	cfg := loadedConfig()
	current, exists := findProjectByID(cfg, projectID)
	if !exists {
		return projectNotFoundJSON()
	}
	items, errPersist := persistSmartKeyMutation(
		req.HostCallbackID,
		"delete",
		smartKeyMatchField(current),
		smartKeyMatchValue(current),
		nil,
	)
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedSmartKeys(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return managementJSON(http.StatusOK, map[string]any{
		"deleted": true,
		"id":      current.ID,
	})
}

func rotateProject(req rpcManagementRequest) ([]byte, error) {
	var input projectIdentityRequest
	if failure := decodeProjectManagementBody(req.Body, &input); failure != nil {
		return projectFailureJSON(*failure)
	}
	cfg := loadedConfig()
	current, exists := findProjectByID(cfg, input.ID)
	if !exists {
		return projectNotFoundJSON()
	}
	if strings.EqualFold(current.Status, projectStatusRevoked) {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_project_revoked",
			Message: "A revoked project cannot be rotated.",
			Status:  http.StatusConflict,
		})
	}
	plaintext, digest, errKey := generateProjectKey()
	if errKey != nil {
		return projectFailureJSON(projectFailure{
			Code:    "bravo_project_key_generation_failed",
			Message: "Could not generate a project key.",
			Status:  http.StatusInternalServerError,
		})
	}
	updated := cloneSmartKeyConfig(current)
	updated.SHA256 = digest
	updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	items, errPersist := persistSmartKeyMutation(
		req.HostCallbackID,
		"replace",
		smartKeyMatchField(current),
		smartKeyMatchValue(current),
		&updated,
	)
	if errPersist != nil {
		return projectHostFailureJSON(errPersist)
	}
	if errInstall := installPersistedSmartKeys(items); errInstall != nil {
		return projectRuntimeInstallFailureJSON(errInstall)
	}
	return managementJSON(http.StatusOK, map[string]any{
		"project":       smartKeyProjectView(updated),
		"plaintext_key": plaintext,
		"project_api": map[string]any{
			"limits": projectLimitsDocs(),
			"routes": projectRoutesDocs(),
		},
	})
}

type projectFailure struct {
	Code    string
	Message string
	Status  int
}

func decodeProjectManagementBody(body []byte, value any) *projectFailure {
	if len(body) == 0 {
		return &projectFailure{
			Code:    "bravo_project_body_required",
			Message: "A JSON request body is required.",
			Status:  http.StatusBadRequest,
		}
	}
	if len(body) > maxProjectManagementBodyBytes {
		return &projectFailure{
			Code:    "bravo_project_body_too_large",
			Message: "The project request body is too large.",
			Status:  http.StatusRequestEntityTooLarge,
		}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return &projectFailure{
			Code:    "bravo_project_body_invalid",
			Message: "The project request body must be a JSON object.",
			Status:  http.StatusBadRequest,
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(value); errDecode != nil {
		return &projectFailure{
			Code:    "bravo_project_body_invalid",
			Message: "The project request body must be a valid JSON object with known fields.",
			Status:  http.StatusBadRequest,
		}
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return &projectFailure{
			Code:    "bravo_project_body_invalid",
			Message: "The project request body must contain exactly one JSON object.",
			Status:  http.StatusBadRequest,
		}
	}
	return nil
}

func normalizeProjectName(value string) (string, *projectFailure) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &projectFailure{
			Code:    "bravo_project_name_required",
			Message: "Project name is required.",
			Status:  http.StatusBadRequest,
		}
	}
	if len([]rune(value)) > 120 {
		return "", &projectFailure{
			Code:    "bravo_project_name_too_long",
			Message: "Project name must be at most 120 characters.",
			Status:  http.StatusBadRequest,
		}
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", &projectFailure{
				Code:    "bravo_project_name_invalid",
				Message: "Project name cannot contain control or invisible formatting characters.",
				Status:  http.StatusBadRequest,
			}
		}
	}
	return value, nil
}

func normalizeProjectModels(cfg pluginConfig, values []string, defaultAll bool) ([]string, *projectFailure) {
	if len(values) == 0 && defaultAll {
		return []string{"*"}, nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "/"))
		prefix := strings.ToLower(strings.Trim(cfg.Prefix, "/"))
		if prefix != "" && strings.HasPrefix(value, prefix+"/") {
			value = strings.TrimPrefix(value, prefix+"/")
		}
		if value == "" {
			continue
		}
		if value != "*" {
			if _, exists := cfg.Models[value]; !exists {
				return nil, &projectFailure{
					Code:    "bravo_project_model_unknown",
					Message: "Unknown Bravo logical model: " + value,
					Status:  http.StatusBadRequest,
				}
			}
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, &projectFailure{
			Code:    "bravo_project_models_required",
			Message: "At least one model or * is required.",
			Status:  http.StatusBadRequest,
		}
	}
	return out, nil
}

func normalizeProjectState(enabledValue *bool, statusValue string, defaultEnabled bool) (bool, string, *projectFailure) {
	enabled := defaultEnabled
	if enabledValue != nil {
		enabled = *enabledValue
	}
	status := strings.ToLower(strings.TrimSpace(statusValue))
	if status == "" {
		if enabled {
			status = projectStatusActive
		} else {
			status = projectStatusDisabled
		}
	}
	switch status {
	case projectStatusActive:
		if enabledValue == nil {
			enabled = true
		}
		if !enabled {
			return false, "", &projectFailure{
				Code:    "bravo_project_state_conflict",
				Message: "active status conflicts with enabled=false.",
				Status:  http.StatusBadRequest,
			}
		}
	case projectStatusDisabled, projectStatusRevoked:
		if enabledValue == nil {
			enabled = false
		}
		if enabled {
			return false, "", &projectFailure{
				Code:    "bravo_project_state_conflict",
				Message: status + " status conflicts with enabled=true.",
				Status:  http.StatusBadRequest,
			}
		}
	default:
		return false, "", &projectFailure{
			Code:    "bravo_project_status_invalid",
			Message: "status must be active, disabled, or revoked.",
			Status:  http.StatusBadRequest,
		}
	}
	return enabled, status, nil
}

func generateUniqueProjectID(cfg pluginConfig) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		random := make([]byte, 12)
		if _, errRead := rand.Read(random); errRead != nil {
			return "", errRead
		}
		candidateID := "prj_" + base64.RawURLEncoding.EncodeToString(random)
		if _, exists := findProjectByID(cfg, candidateID); !exists {
			return candidateID, nil
		}
	}
	return "", fmt.Errorf("project id collision budget exhausted")
}

func generateProjectKey() (plaintext string, digest string, err error) {
	random := make([]byte, 36)
	if _, errRead := rand.Read(random); errRead != nil {
		return "", "", errRead
	}
	plaintext = "brv_" + base64.RawURLEncoding.EncodeToString(random)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, hex.EncodeToString(sum[:]), nil
}

func findProjectByID(cfg pluginConfig, projectID string) (smartKeyConfig, bool) {
	projectID = strings.TrimSpace(projectID)
	for _, item := range cfg.SmartKeys {
		if item.ID == projectID {
			return item, true
		}
	}
	return smartKeyConfig{}, false
}

func findProjectByName(cfg pluginConfig, name string) (smartKeyConfig, bool) {
	name = strings.TrimSpace(name)
	for _, item := range cfg.SmartKeys {
		if strings.EqualFold(item.Name, name) {
			return item, true
		}
	}
	return smartKeyConfig{}, false
}

func smartKeyProjectView(item smartKeyConfig) projectView {
	status := strings.ToLower(strings.TrimSpace(item.Status))
	if status == "" {
		if smartKeyEnabled(item) {
			status = projectStatusActive
		} else {
			status = projectStatusDisabled
		}
	}
	summary := projectUsageSummary(item.ID, time.Now())
	return projectView{
		ID:             item.ID,
		Name:           item.Name,
		Enabled:        smartKeyEnabled(item),
		Status:         status,
		Models:         append([]string{}, item.Models...),
		PrimaryAuthIDs: append([]string{}, item.PrimaryAuthIDs...),
		AllowedAuthIDs: append([]string{}, item.AllowedAuthIDs...),
		Policy:         cloneProjectPolicy(item.Policy),
		PromptCache:    projectPromptCacheViewFor(item),
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
		Usage:          summary,
	}
}

func cloneSmartKeyConfig(item smartKeyConfig) smartKeyConfig {
	item.Models = append([]string(nil), item.Models...)
	item.PrimaryAuthIDs = append([]string(nil), item.PrimaryAuthIDs...)
	item.AllowedAuthIDs = append([]string(nil), item.AllowedAuthIDs...)
	item.Policy = cloneProjectPolicy(item.Policy)
	if item.Enabled != nil {
		enabled := *item.Enabled
		item.Enabled = &enabled
	}
	return item
}

func cloneProjectPolicy(policy map[string]any) map[string]any {
	if len(policy) == 0 {
		return map[string]any{}
	}
	raw, errMarshal := json.Marshal(policy)
	if errMarshal != nil {
		return map[string]any{}
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func projectPromptCacheViewFor(item smartKeyConfig) projectPromptCacheView {
	policy, _ := normalizeProjectPromptCachePolicy(item.Policy)
	return projectPromptCacheView{
		AnthropicTTL: policy.AnthropicTTL,
		OpenAIMode:   projectPromptCacheOpenAIManaged,
	}
}

func setProjectPromptCachePolicy(policy map[string]any, input projectPromptCachePolicy) *projectFailure {
	normalized, failure := normalizeProjectPromptCacheInput(input)
	if failure != nil {
		return failure
	}
	if policy == nil {
		return &projectFailure{
			Code:    "bravo_project_policy_invalid",
			Message: "project policy storage is unavailable.",
			Status:  http.StatusInternalServerError,
		}
	}
	policy["prompt_cache"] = map[string]any{
		"anthropic_ttl": normalized.AnthropicTTL,
	}
	return nil
}

// validateAndCanonicalizeProjectPromptCachePolicy runs before the host mutation
// callback so a malformed generic policy can never be persisted and poison the
// next Bravo startup. The dedicated prompt_cache request field and the generic
// policy path intentionally share the same validator.
func validateAndCanonicalizeProjectPromptCachePolicy(policy map[string]any) *projectFailure {
	if policy == nil {
		return nil
	}
	normalized, failure := normalizeProjectPromptCachePolicy(policy)
	if failure != nil {
		return failure
	}
	if _, exists := policy["prompt_cache"]; exists {
		policy["prompt_cache"] = map[string]any{
			"anthropic_ttl": normalized.AnthropicTTL,
		}
	}
	return nil
}

func normalizeProjectPromptCachePolicy(policy map[string]any) (projectPromptCachePolicy, *projectFailure) {
	defaultPolicy := projectPromptCachePolicy{AnthropicTTL: projectPromptCacheTTLAutomatic}
	if len(policy) == 0 {
		return defaultPolicy, nil
	}
	rawValue, exists := policy["prompt_cache"]
	if !exists || rawValue == nil {
		return defaultPolicy, nil
	}
	raw, errMarshal := json.Marshal(rawValue)
	if errMarshal != nil {
		return projectPromptCachePolicy{}, invalidProjectPromptCachePolicy()
	}
	var input projectPromptCachePolicy
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&input); errDecode != nil {
		return projectPromptCachePolicy{}, invalidProjectPromptCachePolicy()
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return projectPromptCachePolicy{}, invalidProjectPromptCachePolicy()
	}
	return normalizeProjectPromptCacheInput(input)
}

func normalizeProjectPromptCacheInput(input projectPromptCachePolicy) (projectPromptCachePolicy, *projectFailure) {
	input.AnthropicTTL = strings.ToLower(strings.TrimSpace(input.AnthropicTTL))
	if input.AnthropicTTL == "" {
		input.AnthropicTTL = projectPromptCacheTTLAutomatic
	}
	switch input.AnthropicTTL {
	case projectPromptCacheTTLAutomatic, projectPromptCacheTTL5Minutes, projectPromptCacheTTL1Hour:
		return input, nil
	default:
		return projectPromptCachePolicy{}, invalidProjectPromptCachePolicy()
	}
}

func invalidProjectPromptCachePolicy() *projectFailure {
	return &projectFailure{
		Code:    "bravo_project_prompt_cache_invalid",
		Message: "prompt_cache.anthropic_ttl must be auto, 5m, or 1h.",
		Status:  http.StatusBadRequest,
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func smartKeyMatchField(item smartKeyConfig) string {
	if item.LegacyDerivedID {
		return "sha256"
	}
	return "id"
}

func smartKeyMatchValue(item smartKeyConfig) string {
	if smartKeyMatchField(item) == "sha256" {
		return item.SHA256
	}
	return item.ID
}

func persistSmartKeyMutation(
	hostCallbackID string,
	operation string,
	matchField string,
	matchValue string,
	value *smartKeyConfig,
) ([]smartKeyConfig, error) {
	request := hostPluginConfigListMutationRequest{
		HostCallbackID: strings.TrimSpace(hostCallbackID),
		Field:          "smart_keys",
		Operation:      operation,
		MatchField:     matchField,
		MatchValue:     matchValue,
		UniqueFields:   []string{"id", "name", "sha256"},
	}
	if value != nil {
		raw, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return nil, errMarshal
		}
		request.Value = raw
	}
	raw, errCall := callHost(pluginabi.MethodHostPluginConfigListMutate, request)
	if errCall != nil {
		return nil, errCall
	}
	var response hostPluginConfigListMutationResult
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode persisted smart keys: %w", errUnmarshal)
	}
	items := make([]smartKeyConfig, 0, len(response.Items))
	for index, rawItem := range response.Items {
		var item smartKeyConfig
		if errUnmarshal := json.Unmarshal(rawItem, &item); errUnmarshal != nil {
			return nil, fmt.Errorf("decode persisted smart key %d: %w", index, errUnmarshal)
		}
		items = append(items, item)
	}
	return items, nil
}

func installPersistedSmartKeys(items []smartKeyConfig) error {
	cfg := loadedConfig()
	cfg.SmartKeys = append([]smartKeyConfig(nil), items...)
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		return fmt.Errorf("validate persisted Bravo projects: %w", errNormalize)
	}
	currentConfig.Store(cfg)
	return nil
}

func projectRuntimeInstallFailureJSON(err error) ([]byte, error) {
	message := "The project was persisted but could not be installed into the Bravo runtime."
	if err != nil {
		message += " Review the server log before retrying."
	}
	return projectFailureJSON(projectFailure{
		Code:    "bravo_project_runtime_install_failed",
		Message: message,
		Status:  http.StatusInternalServerError,
	})
}

func projectHostFailureJSON(err error) ([]byte, error) {
	failure := projectFailure{
		Code:    "bravo_project_persistence_failed",
		Message: "Could not persist the Bravo project.",
		Status:  http.StatusBadGateway,
	}
	if hostErr, ok := err.(*hostCallError); ok {
		if strings.TrimSpace(hostErr.Code) != "" {
			failure.Code = hostErr.Code
		}
		if strings.TrimSpace(hostErr.Message) != "" {
			failure.Message = hostErr.Message
		}
		if hostErr.HTTPStatus >= 400 && hostErr.HTTPStatus <= 599 {
			failure.Status = hostErr.HTTPStatus
		}
	}
	return projectFailureJSON(failure)
}

func projectNotFoundJSON() ([]byte, error) {
	return projectFailureJSON(projectFailure{
		Code:    "bravo_project_not_found",
		Message: "Bravo project was not found.",
		Status:  http.StatusNotFound,
	})
}

func projectFailureJSON(failure projectFailure) ([]byte, error) {
	failure = clientProjectFailureRU(failure)
	return managementJSON(failure.Status, map[string]any{
		"error": map[string]any{
			"code":    failure.Code,
			"message": failure.Message,
		},
	})
}

func firstQueryValue(query map[string][]string, key string) string {
	values := query[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
