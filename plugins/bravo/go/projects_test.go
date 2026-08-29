package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProjectAdaptiveAssistDefaultsFalseAndPatchIsOptional(t *testing.T) {
	view := smartKeyProjectView(smartKeyConfig{ID: "legacy", Name: "Legacy"})
	if view.AdaptiveAssist {
		t.Fatal("legacy project unexpectedly opted into assist")
	}
	var patch patchProjectRequest
	if err := json.Unmarshal([]byte(`{"id":"legacy","name":"unchanged"}`), &patch); err != nil {
		t.Fatal(err)
	}
	if patch.AdaptiveAssist != nil {
		t.Fatal("omitted adaptive_assist would overwrite persisted value")
	}
	if err := json.Unmarshal([]byte(`{"id":"legacy","adaptive_assist":false}`), &patch); err != nil || patch.AdaptiveAssist == nil || *patch.AdaptiveAssist {
		t.Fatalf("explicit false patch lost pointer semantics: err=%v patch=%#v", err, patch)
	}
}

func TestBravoProjectCRUDGeneratesOneTimeHashedKeys(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.RequireSmartKey = true
	cfg.AdaptiveAllocatorMode = "assist"
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
	})

	var stored []json.RawMessage
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			return mustBravoJSON(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
				ID:        "claude-primary",
				AuthIndex: "1111111111111111",
				Name:      "claude-primary.json",
				Provider:  "claude",
			}}}), nil
		}
		if method != pluginabi.MethodHostPluginConfigListMutate {
			t.Fatalf("unexpected host method %q", method)
		}
		rawPayload := mustBravoJSON(t, payload)
		if strings.Contains(string(rawPayload), "brv_") {
			t.Fatal("plaintext project key leaked into the persistence callback")
		}
		var req hostPluginConfigListMutationRequest
		decodeBravoPayload(t, payload, &req)
		match := -1
		for index, rawItem := range stored {
			var item map[string]any
			if errUnmarshal := json.Unmarshal(rawItem, &item); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if value, _ := item[req.MatchField].(string); value == req.MatchValue {
				match = index
				break
			}
		}
		switch req.Operation {
		case "append":
			if match >= 0 {
				t.Fatal("append matched an existing item")
			}
			stored = append(stored, append(json.RawMessage(nil), req.Value...))
		case "replace":
			if match < 0 {
				t.Fatal("replace did not match an item")
			}
			stored[match] = append(json.RawMessage(nil), req.Value...)
		case "delete":
			if match < 0 {
				t.Fatal("delete did not match an item")
			}
			stored = append(stored[:match], stored[match+1:]...)
		default:
			t.Fatalf("unexpected operation %q", req.Operation)
		}
		return mustBravoJSON(t, hostPluginConfigListMutationResult{Items: stored}), nil
	})

	status, created := callProjectManagement(t, http.MethodPost, "/v0/management/bravo/projects", `{
		"name":"Alpha",
		"models":["bravo/frontier"],
		"primary_auth_ids":["claude-primary"],
		"allowed_auth_ids":["claude-primary"],
		"adaptive_assist":true,
		"policy":{"future_reserve_percent":50},
		"prompt_cache":{"anthropic_ttl":"1h"}
	}`)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d body=%#v", status, created)
	}
	plaintext, _ := created["plaintext_key"].(string)
	if !strings.HasPrefix(plaintext, "brv_") {
		t.Fatalf("plaintext_key = %q", plaintext)
	}
	projectAPI := projectMap(t, created["project_api"])
	limitsAPI := projectMap(t, projectAPI["limits"])
	routesAPI := projectMap(t, projectAPI["routes"])
	if limitsAPI["endpoint"] != projectLimitsPublicPath || routesAPI["endpoint"] != projectRoutesPublicPath {
		t.Fatalf("project_api = %#v", projectAPI)
	}
	project := projectMap(t, created["project"])
	projectID, _ := project["id"].(string)
	if !strings.HasPrefix(projectID, "prj_") || project["name"] != "Alpha" {
		t.Fatalf("created project = %#v", project)
	}
	if _, leaked := project["sha256"]; leaked {
		t.Fatal("create response exposed sha256")
	}
	if got, _ := project["allowed_auth_ids"].([]any); len(got) != 1 || got[0] != "1111111111111111" {
		t.Fatalf("created allowed_auth_ids = %#v", project["allowed_auth_ids"])
	}
	if project["adaptive_assist"] != true {
		t.Fatalf("created adaptive_assist = %#v", project["adaptive_assist"])
	}
	promptCache := projectMap(t, project["prompt_cache"])
	if promptCache["anthropic_ttl"] != "1h" || promptCache["openai_mode"] != projectPromptCacheOpenAIManaged {
		t.Fatalf("created prompt_cache = %#v", promptCache)
	}
	if len(stored) != 1 {
		t.Fatalf("stored items = %d", len(stored))
	}
	var persisted smartKeyConfig
	if errUnmarshal := json.Unmarshal(stored[0], &persisted); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	sum := sha256.Sum256([]byte(plaintext))
	if persisted.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("persisted digest does not match the one-time plaintext key")
	}
	if !persisted.AdaptiveAssist {
		t.Fatal("create did not persist adaptive_assist")
	}
	if strings.Contains(string(stored[0]), plaintext) {
		t.Fatal("persisted config contains plaintext key")
	}

	status, listed := callProjectManagement(t, http.MethodGet, "/v0/management/bravo/projects", "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	listedRaw := string(mustBravoJSON(t, listed))
	if strings.Contains(listedRaw, plaintext) ||
		strings.Contains(listedRaw, persisted.SHA256) ||
		strings.Contains(listedRaw, `"sha256"`) {
		t.Fatal("project list exposed key material")
	}
	if projects, _ := listed["projects"].([]any); len(projects) != 1 {
		t.Fatalf("projects = %#v", listed["projects"])
	} else if listedProject := projectMap(t, projects[0]); listedProject["adaptive_assist"] != true {
		t.Fatalf("listed adaptive_assist = %#v", listedProject["adaptive_assist"])
	}
	modelOptions, _ := listed["models"].([]any)
	if len(modelOptions) != len(cfg.Models) {
		t.Fatalf("model options = %d, want %d", len(modelOptions), len(cfg.Models))
	}
	var frontierOption map[string]any
	for _, rawOption := range modelOptions {
		option := projectMap(t, rawOption)
		if option["id"] == "frontier" {
			frontierOption = option
			break
		}
	}
	if frontierOption == nil || frontierOption["request_model"] != "bravo/frontier" {
		t.Fatalf("frontier model option = %#v", frontierOption)
	}

	auth := authenticateProjectKey(t, plaintext)
	if !auth.Authenticated || auth.Principal != "bravo:"+projectID {
		t.Fatalf("auth response = %#v", auth)
	}
	if auth.Metadata[bravoProjectIDMetadataKey] != projectID {
		t.Fatalf("auth metadata = %#v", auth.Metadata)
	}
	if auth.Metadata[bravoPromptCacheTTLMetadataKey] != "1h" {
		t.Fatalf("auth prompt cache metadata = %#v", auth.Metadata)
	}

	status, patched := callProjectManagement(
		t,
		http.MethodPatch,
		"/v0/management/bravo/projects",
		`{"id":"`+projectID+`","name":"Alpha disabled","enabled":false,"adaptive_assist":false,"policy":null,"prompt_cache":{"anthropic_ttl":"5m"}}`,
	)
	if status != http.StatusOK {
		t.Fatalf("patch status = %d body=%#v", status, patched)
	}
	patchedProject := projectMap(t, patched["project"])
	if patchedProject["enabled"] != false || patchedProject["status"] != projectStatusDisabled {
		t.Fatalf("patched project = %#v", patchedProject)
	}
	if patchedProject["adaptive_assist"] != false {
		t.Fatalf("patched adaptive_assist = %#v", patchedProject["adaptive_assist"])
	}
	var patchedPersisted smartKeyConfig
	if errUnmarshal := json.Unmarshal(stored[0], &patchedPersisted); errUnmarshal != nil || patchedPersisted.AdaptiveAssist {
		t.Fatalf("patch did not persist assist kill switch: err=%v item=%#v", errUnmarshal, patchedPersisted)
	}
	loadedAfterKill := loadedConfig()
	storedAfterKill, foundAfterKill := findProjectByID(loadedAfterKill, projectID)
	if !foundAfterKill || adaptiveConfigForProject(loadedAfterKill, storedAfterKill).AdaptiveAllocatorMode == "assist" {
		t.Fatalf("hot assist kill switch was not installed: found=%v project=%#v", foundAfterKill, storedAfterKill)
	}
	patchedPromptCache := projectMap(t, patchedProject["prompt_cache"])
	if patchedPromptCache["anthropic_ttl"] != "5m" {
		t.Fatalf("patched prompt_cache = %#v", patchedPromptCache)
	}
	if authenticateProjectKey(t, plaintext).Authenticated {
		t.Fatal("disabled project key still authenticates")
	}

	status, rotated := callProjectManagement(
		t,
		http.MethodPost,
		"/v0/management/bravo/projects/rotate",
		`{"id":"`+projectID+`"}`,
	)
	if status != http.StatusOK {
		t.Fatalf("rotate status = %d body=%#v", status, rotated)
	}
	rotatedProject := projectMap(t, rotated["project"])
	if got, _ := rotatedProject["allowed_auth_ids"].([]any); len(got) != 1 || got[0] != "1111111111111111" {
		t.Fatalf("rotation lost allowed_auth_ids: %#v", rotatedProject)
	}
	rotatedPlaintext, _ := rotated["plaintext_key"].(string)
	if !strings.HasPrefix(rotatedPlaintext, "brv_") || rotatedPlaintext == plaintext {
		t.Fatalf("rotated plaintext_key = %q", rotatedPlaintext)
	}
	if projectAPI := projectMap(t, rotated["project_api"]); projectMap(t, projectAPI["limits"])["endpoint"] != projectLimitsPublicPath {
		t.Fatalf("rotated project_api = %#v", projectAPI)
	}
	if authenticateProjectKey(t, plaintext).Authenticated {
		t.Fatal("old key authenticates after rotation")
	}

	status, _ = callProjectManagement(
		t,
		http.MethodPatch,
		"/v0/management/bravo/projects",
		`{"id":"`+projectID+`","enabled":true}`,
	)
	if status != http.StatusOK {
		t.Fatalf("enable status = %d", status)
	}
	if !authenticateProjectKey(t, rotatedPlaintext).Authenticated {
		t.Fatal("rotated key did not authenticate after enabling the project")
	}

	status, deleted := callProjectManagement(
		t,
		http.MethodDelete,
		"/v0/management/bravo/projects",
		`{"id":"`+projectID+`"}`,
	)
	if status != http.StatusOK || deleted["deleted"] != true {
		t.Fatalf("delete status/body = %d %#v", status, deleted)
	}
	if authenticateProjectKey(t, rotatedPlaintext).Authenticated {
		t.Fatal("deleted project key still authenticates")
	}
}

func TestBravoProjectPromptCachePolicyIsValidatedAndLegacySafe(t *testing.T) {
	legacy := smartKeyConfig{Policy: map[string]any{}}
	view := projectPromptCacheViewFor(legacy)
	if view.AnthropicTTL != projectPromptCacheTTLAutomatic ||
		view.OpenAIMode != projectPromptCacheOpenAIManaged {
		t.Fatalf("legacy prompt cache view = %#v", view)
	}

	for _, value := range []string{"10m", "24h", "forever"} {
		_, failure := normalizeProjectPromptCacheInput(projectPromptCachePolicy{AnthropicTTL: value})
		if failure == nil || failure.Code != "bravo_project_prompt_cache_invalid" {
			t.Fatalf("TTL %q failure = %#v", value, failure)
		}
	}

	policy := map[string]any{"future": true}
	if failure := setProjectPromptCachePolicy(policy, projectPromptCachePolicy{AnthropicTTL: " 1H "}); failure != nil {
		t.Fatalf("set valid policy failed: %#v", failure)
	}
	normalized, failure := normalizeProjectPromptCachePolicy(policy)
	if failure != nil || normalized.AnthropicTTL != "1h" || policy["future"] != true {
		t.Fatalf("normalized policy = %#v, failure=%#v, raw=%#v", normalized, failure, policy)
	}

	invalid := defaultPluginConfig()
	invalid.SmartKeys = []smartKeyConfig{{
		ID:      "prj_invalid_cache",
		Name:    "Invalid cache",
		SHA256:  strings.Repeat("c", 64),
		Enabled: boolPointer(true),
		Status:  projectStatusActive,
		Models:  []string{"*"},
		Policy: map[string]any{
			"prompt_cache": map[string]any{"anthropic_ttl": "10m"},
		},
	}}
	if errNormalize := normalizeConfig(&invalid); errNormalize == nil ||
		!strings.Contains(errNormalize.Error(), "invalid prompt cache policy") {
		t.Fatalf("invalid config error = %v", errNormalize)
	}
}

func TestBravoProjectPromptCachePolicyFailsBeforePersistence(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
	})

	hostCalls := 0
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		hostCalls++
		t.Fatalf("invalid prompt cache policy called host method %q with %#v", method, payload)
		return nil, nil
	})

	status, response := callProjectManagement(
		t,
		http.MethodPost,
		"/v0/management/bravo/projects",
		`{"name":"Invalid cache","models":["*"],"policy":{"prompt_cache":{"anthropic_ttl":"24h"}}}`,
	)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid cache status/body = %d %#v", status, response)
	}
	errorBody := projectMap(t, response["error"])
	if errorBody["code"] != "bravo_project_prompt_cache_invalid" {
		t.Fatalf("invalid cache error = %#v", errorBody)
	}
	if hostCalls != 0 {
		t.Fatalf("host calls = %d, want zero", hostCalls)
	}
}

func TestBravoProjectManagementRoutesAreProtectedManagementRoutes(t *testing.T) {
	raw, errRegister := registerManagement()
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var registration managementRegistrationResponse
	if errUnmarshal := json.Unmarshal(env.Result, &registration); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	expected := map[string]bool{
		http.MethodGet + " /bravo/projects":         false,
		http.MethodPost + " /bravo/projects":        false,
		http.MethodPatch + " /bravo/projects":       false,
		http.MethodDelete + " /bravo/projects":      false,
		http.MethodPost + " /bravo/projects/rotate": false,
	}
	for _, route := range registration.Routes {
		key := route.Method + " " + route.Path
		if _, exists := expected[key]; exists {
			expected[key] = true
		}
	}
	for route, found := range expected {
		if !found {
			t.Errorf("missing management route %s", route)
		}
	}
	for _, resource := range registration.Resources {
		if strings.Contains(resource.Path, "projects") {
			t.Fatalf("project mutation route was registered as public resource: %#v", resource)
		}
	}
}

func TestDecodeProjectManagementBodyIsStrict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"name":"Alpha","unexpected":true}`},
		{name: "trailing object", body: `{"name":"Alpha"}{"name":"Beta"}`},
		{name: "trailing garbage", body: `{"name":"Alpha"} nope`},
		{name: "null", body: `null`},
		{name: "array", body: `[]`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var request createProjectRequest
			failure := decodeProjectManagementBody([]byte(test.body), &request)
			if failure == nil || failure.Status != http.StatusBadRequest {
				t.Fatalf("decode failure = %#v", failure)
			}
		})
	}

	var request createProjectRequest
	if failure := decodeProjectManagementBody(
		[]byte(" \n\t"+`{"name":"Alpha","policy":{"provider_specific":true}}`+" \n"),
		&request,
	); failure != nil {
		t.Fatalf("valid request failed: %#v", failure)
	}
	if request.Name != "Alpha" || request.Policy["provider_specific"] != true {
		t.Fatalf("decoded request = %#v", request)
	}
}

func TestProjectNamesRejectSpoofingAndMatchCaseInsensitively(t *testing.T) {
	for _, name := range []string{
		"line\nbreak",
		"hidden\u200fmark",
		"override\u202ename",
	} {
		if _, failure := normalizeProjectName(name); failure == nil ||
			failure.Code != "bravo_project_name_invalid" {
			t.Fatalf("normalizeProjectName(%q) failure = %#v", name, failure)
		}
	}

	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{{
		ID:      "prj_existing",
		Name:    "Alpha",
		SHA256:  strings.Repeat("a", 64),
		Enabled: boolPointer(true),
		Status:  projectStatusActive,
		Models:  []string{"*"},
	}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	if project, exists := findProjectByName(cfg, "alpha"); !exists || project.ID != "prj_existing" {
		t.Fatalf("case-insensitive project lookup = %#v, %v", project, exists)
	}
}

func TestRevokedProjectCannotBeCreatedOrReenabled(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{{
		ID:      "prj_revoked",
		Name:    "Revoked",
		SHA256:  strings.Repeat("b", 64),
		Enabled: boolPointer(false),
		Status:  projectStatusRevoked,
		Models:  []string{"*"},
	}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
	})
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		t.Fatalf("revoked state validation unexpectedly called host method %q with %#v", method, payload)
		return nil, nil
	})

	status, response := callProjectManagement(
		t,
		http.MethodPatch,
		"/v0/management/bravo/projects",
		`{"id":"prj_revoked","enabled":true}`,
	)
	if status != http.StatusConflict {
		t.Fatalf("revoked enable status/body = %d %#v", status, response)
	}
	errorBody := projectMap(t, response["error"])
	if errorBody["code"] != "bravo_project_revoked" {
		t.Fatalf("revoked enable error = %#v", errorBody)
	}

	status, response = callProjectManagement(
		t,
		http.MethodPost,
		"/v0/management/bravo/projects",
		`{"name":"Starts revoked","enabled":false,"status":"revoked","models":["*"]}`,
	)
	if status != http.StatusBadRequest {
		t.Fatalf("revoked create status/body = %d %#v", status, response)
	}
	errorBody = projectMap(t, response["error"])
	if errorBody["code"] != "bravo_project_state_invalid" {
		t.Fatalf("revoked create error = %#v", errorBody)
	}
}

func TestLegacySmartKeyIDMigratesOnceAndSurvivesRotation(t *testing.T) {
	const legacyPlaintext = "brv_legacy_project"
	legacyDigest := sha256.Sum256([]byte(legacyPlaintext))
	legacyDigestHex := hex.EncodeToString(legacyDigest[:])
	legacyRaw := mustBravoJSON(t, map[string]any{
		"name":    "Legacy",
		"sha256":  legacyDigestHex,
		"models":  []string{"*"},
		"enabled": true,
		"status":  projectStatusActive,
	})

	previousConfig := loadedConfig()
	cfg := defaultPluginConfig()
	cfg.SmartKeys = []smartKeyConfig{{
		Name:    "Legacy",
		SHA256:  legacyDigestHex,
		Models:  []string{"*"},
		Enabled: boolPointer(true),
		Status:  projectStatusActive,
	}}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	legacyID := cfg.SmartKeys[0].ID
	if legacyID == "" || !cfg.SmartKeys[0].LegacyDerivedID {
		t.Fatalf("legacy project was not assigned a derived id: %#v", cfg.SmartKeys[0])
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
	})

	stored := []json.RawMessage{legacyRaw}
	var mutationMatches []string
	installBravoHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostPluginConfigListMutate {
			t.Fatalf("unexpected host method %q", method)
		}
		var req hostPluginConfigListMutationRequest
		decodeBravoPayload(t, payload, &req)
		mutationMatches = append(mutationMatches, req.MatchField)
		match := -1
		for index, rawItem := range stored {
			var item map[string]any
			if errUnmarshal := json.Unmarshal(rawItem, &item); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if value, _ := item[req.MatchField].(string); value == req.MatchValue {
				match = index
				break
			}
		}
		if req.Operation != "replace" || match < 0 {
			t.Fatalf("legacy mutation = %#v, match=%d", req, match)
		}
		stored[match] = append(json.RawMessage(nil), req.Value...)
		return mustBravoJSON(t, hostPluginConfigListMutationResult{Items: stored}), nil
	})

	status, patched := callProjectManagement(
		t,
		http.MethodPatch,
		"/v0/management/bravo/projects",
		`{"id":"`+legacyID+`","name":"Legacy migrated"}`,
	)
	if status != http.StatusOK {
		t.Fatalf("legacy patch status = %d body=%#v", status, patched)
	}
	var migrated smartKeyConfig
	if errUnmarshal := json.Unmarshal(stored[0], &migrated); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if migrated.ID != legacyID {
		t.Fatalf("migrated id = %q, want %q", migrated.ID, legacyID)
	}
	if loadedConfig().SmartKeys[0].LegacyDerivedID {
		t.Fatal("persisted project still marked as an in-memory legacy id")
	}

	status, rotated := callProjectManagement(
		t,
		http.MethodPost,
		"/v0/management/bravo/projects/rotate",
		`{"id":"`+legacyID+`"}`,
	)
	if status != http.StatusOK {
		t.Fatalf("legacy rotate status = %d body=%#v", status, rotated)
	}
	if len(mutationMatches) != 2 || mutationMatches[0] != "sha256" || mutationMatches[1] != "id" {
		t.Fatalf("legacy mutation match fields = %#v", mutationMatches)
	}
	rotatedProject := projectMap(t, rotated["project"])
	if rotatedProject["id"] != legacyID {
		t.Fatalf("rotated project id = %#v, want %q", rotatedProject["id"], legacyID)
	}
	if authenticateProjectKey(t, legacyPlaintext).Authenticated {
		t.Fatal("legacy plaintext authenticates after rotation")
	}
	rotatedPlaintext, _ := rotated["plaintext_key"].(string)
	auth := authenticateProjectKey(t, rotatedPlaintext)
	if !auth.Authenticated || auth.Principal != "bravo:"+legacyID {
		t.Fatalf("rotated legacy auth = %#v", auth)
	}
}

func callProjectManagement(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	raw, errHandle := handleManagement(mustJSONValue(t, rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{
			Method: method,
			Path:   path,
			Body:   []byte(body),
		},
		HostCallbackID: "management-callback",
	}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("plugin envelope error = %#v", env.Error)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	bodyValue := make(map[string]any)
	if len(response.Body) > 0 {
		if errUnmarshal := json.Unmarshal(response.Body, &bodyValue); errUnmarshal != nil {
			t.Fatalf("decode management body: %v; body=%s", errUnmarshal, response.Body)
		}
	}
	return response.StatusCode, bodyValue
}

func projectMap(t *testing.T, value any) map[string]any {
	t.Helper()
	project, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("project = %#v", value)
	}
	return project
}

func authenticateProjectKey(t *testing.T, key string) pluginapi.FrontendAuthResponse {
	t.Helper()
	raw, errAuthenticate := authenticateSmartKey(mustJSONValue(t, pluginapi.FrontendAuthRequest{
		Headers: http.Header{"Authorization": []string{"Bearer " + key}},
	}))
	if errAuthenticate != nil {
		t.Fatal(errAuthenticate)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var response pluginapi.FrontendAuthResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return response
}
