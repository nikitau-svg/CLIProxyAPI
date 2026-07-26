package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestExtractClaudeCodeSessionIDFromPayloadJSON(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"cache-session-1\"}"}}`)
	got := ExtractClaudeCodeSessionID(context.Background(), payload, nil)
	if got != "cache-session-1" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want cache-session-1", got)
	}
}

func TestExtractClaudeCodeSessionIDFromHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set(ClaudeCodeSessionHeader, "header-session-1")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	got := ExtractClaudeCodeSessionID(ctx, []byte(`{"model":"gpt-5.4"}`), nil)
	if got != "header-session-1" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want header-session-1", got)
	}
}

func TestClaudeCodePromptCacheStableAcrossRequests(t *testing.T) {
	ctx := context.Background()
	payload := []byte(`{"metadata":{"user_id":"{\"session_id\":\"cache-session-2\"}"}}`)
	first, ok, err := ClaudeCodePromptCache(ctx, "grok-composer-2.5-fast", payload, nil)
	if err != nil {
		t.Fatalf("ClaudeCodePromptCache first error: %v", err)
	}
	if !ok || first.ID == "" {
		t.Fatalf("ClaudeCodePromptCache first = %#v, ok=%v, want cached id", first, ok)
	}
	second, ok, err := ClaudeCodePromptCache(ctx, "grok-composer-2.5-fast", payload, nil)
	if err != nil {
		t.Fatalf("ClaudeCodePromptCache second error: %v", err)
	}
	if !ok || second.ID != first.ID {
		t.Fatalf("second cache id = %q, want %q", second.ID, first.ID)
	}
}

func TestExtractClaudeCodeSessionIDPrefersHeaderOverPayload(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"{"session_id":"payload-session"}"}}`)
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "header-session")

	got := ExtractClaudeCodeSessionID(context.Background(), payload, headers)
	if got != "header-session" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want header-session", got)
	}
}

func TestClaudeCodeExecutionScopeAcceptsLowercaseHeaderMapKeys(t *testing.T) {
	headers := http.Header{
		"x-claude-code-session-id": []string{"lower-session"},
		"x-claude-code-agent-id":   []string{"lower-agent"},
	}

	scope, ok := ClaudeCodeExecutionScope(context.Background(), nil, headers)
	if !ok || scope != "claude:lower-session:agent:lower-agent" {
		t.Fatalf("lowercase header scope = %q, %v", scope, ok)
	}
}

func TestClaudeCodeExecutionScopeIsolatesAgents(t *testing.T) {
	rootHeaders := http.Header{}
	rootHeaders.Set(ClaudeCodeSessionHeader, "session-agents")
	childAHeaders := rootHeaders.Clone()
	childAHeaders.Set(ClaudeCodeAgentHeader, "agent-a")
	childBHeaders := rootHeaders.Clone()
	childBHeaders.Set(ClaudeCodeAgentHeader, "agent-b")

	rootScope, ok := ClaudeCodeExecutionScope(context.Background(), nil, rootHeaders)
	if !ok || rootScope != "claude:session-agents:agent:main" {
		t.Fatalf("root scope = %q, %v", rootScope, ok)
	}
	childAScope, ok := ClaudeCodeExecutionScope(context.Background(), nil, childAHeaders)
	if !ok || childAScope != "claude:session-agents:agent:agent-a" {
		t.Fatalf("child A scope = %q, %v", childAScope, ok)
	}
	childBScope, ok := ClaudeCodeExecutionScope(context.Background(), nil, childBHeaders)
	if !ok || childBScope != "claude:session-agents:agent:agent-b" {
		t.Fatalf("child B scope = %q, %v", childBScope, ok)
	}
	if rootScope == childAScope || childAScope == childBScope || rootScope == childBScope {
		t.Fatalf("agent scopes are not isolated: root=%q a=%q b=%q", rootScope, childAScope, childBScope)
	}
}

func TestClaudeCodePromptCacheDeterministicAndAgentScoped(t *testing.T) {
	rootHeaders := http.Header{}
	rootHeaders.Set(ClaudeCodeSessionHeader, "session-cache-agents")
	childHeaders := rootHeaders.Clone()
	childHeaders.Set(ClaudeCodeAgentHeader, "agent-a")

	rootFirst, ok, errFirst := ClaudeCodePromptCache(context.Background(), "gpt-5.4", nil, rootHeaders)
	if errFirst != nil || !ok {
		t.Fatalf("root first cache = %#v, %v, %v", rootFirst, ok, errFirst)
	}
	rootSecond, ok, errSecond := ClaudeCodePromptCache(context.Background(), "gpt-5.4", nil, rootHeaders)
	if errSecond != nil || !ok || rootSecond.ID != rootFirst.ID {
		t.Fatalf("root second cache = %#v, %v, %v; want ID %q", rootSecond, ok, errSecond, rootFirst.ID)
	}
	child, ok, errChild := ClaudeCodePromptCache(context.Background(), "gpt-5.4", nil, childHeaders)
	if errChild != nil || !ok || child.ID == rootFirst.ID {
		t.Fatalf("child cache = %#v, %v, %v; root ID %q", child, ok, errChild, rootFirst.ID)
	}
	otherModel, ok, errModel := ClaudeCodePromptCache(context.Background(), "gpt-5.5", nil, rootHeaders)
	if errModel != nil || !ok || otherModel.ID == rootFirst.ID {
		t.Fatalf("other model cache = %#v, %v, %v; root ID %q", otherModel, ok, errModel, rootFirst.ID)
	}
}

func TestClaudeCodePromptCacheIsolatesBravoProjects(t *testing.T) {
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "shared-session")
	headers.Set(ClaudeCodeAgentHeader, "shared-agent")

	projectAFirst, ok, errAFirst := ClaudeCodePromptCache(bravoPromptCacheContext("project-a"), "gpt-5.6-sol", nil, headers)
	if errAFirst != nil || !ok {
		t.Fatalf("project A first cache = %#v, %v, %v", projectAFirst, ok, errAFirst)
	}
	projectASecond, ok, errASecond := ClaudeCodePromptCache(bravoPromptCacheContext("project-a"), "gpt-5.6-sol", nil, headers)
	if errASecond != nil || !ok || projectASecond.ID != projectAFirst.ID {
		t.Fatalf("project A second cache = %#v, %v, %v; want ID %q", projectASecond, ok, errASecond, projectAFirst.ID)
	}
	projectB, ok, errB := ClaudeCodePromptCache(bravoPromptCacheContext("project-b"), "gpt-5.6-sol", nil, headers)
	if errB != nil || !ok || projectB.ID == projectAFirst.ID {
		t.Fatalf("project B cache = %#v, %v, %v; project A ID %q", projectB, ok, errB, projectAFirst.ID)
	}
}

func TestClaudeCodePromptCachePreservesLegacyIdentityOutsideBravo(t *testing.T) {
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "legacy-session")
	headers.Set(ClaudeCodeAgentHeader, "legacy-agent")

	got, ok, errCache := ClaudeCodePromptCache(nonBravoPromptCacheContext("bravo:not-a-bravo-principal"), "gpt-5.4", nil, headers)
	if errCache != nil || !ok {
		t.Fatalf("legacy cache = %#v, %v, %v", got, ok, errCache)
	}
	executionScope := "claude:legacy-session:agent:legacy-agent"
	legacyIdentity := strings.Join([]string{"cli-proxy-api:codex:claude-code", "gpt-5.4", executionScope}, "\x00")
	want := uuid.NewSHA1(uuid.NameSpaceOID, []byte(legacyIdentity)).String()
	if got.ID != want {
		t.Fatalf("legacy cache ID = %q, want %q", got.ID, want)
	}
}

func bravoPromptCacheContext(projectID string) context.Context {
	return promptCacheAccessContext("plugin:bravo:bravo", "bravo:"+projectID)
}

func nonBravoPromptCacheContext(principal string) context.Context {
	return promptCacheAccessContext("api-key", principal)
}

func promptCacheAccessContext(provider, principal string) context.Context {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Set("accessProvider", provider)
	ginCtx.Set("userApiKey", principal)
	return context.WithValue(context.Background(), "gin", ginCtx)
}
