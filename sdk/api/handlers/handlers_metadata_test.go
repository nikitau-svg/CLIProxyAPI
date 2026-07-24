package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestRequestExecutionMetadataTraceCallbackWebsocketDetection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("skips websocket upgrade", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Connection", "Upgrade")
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		logging.SetGinRequestID(ginCtx, "1234abcd")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)

		if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; exists {
			t.Fatal("unexpected selected auth index callback for websocket upgrade")
		}
	})

	t.Run("keeps callback for incomplete upgrade headers", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		logging.SetGinRequestID(ginCtx, "1234abcd")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)

		if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; !exists {
			t.Fatal("missing selected auth index callback for ordinary HTTP request")
		}
	})
}

func TestRequestExecutionMetadataIncludesSanitizedFrontendAuthContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Set("accessProvider", "plugin:bravo:bravo")
	ginCtx.Set("userApiKey", "sk-plaintext-must-not-leak")
	ginCtx.Set("accessMetadata", map[string]string{
		"bravo_access_provider": "bravo",
		"bravo_project_id":      "prj_primary",
		"bravo_key_name":        "primary",
		"bravo_allowed_models":  "opus,sonnet",
		"principal":             "bravo:primary",
		"api_key":               "sk-plaintext-must-not-leak",
		"access_token":          "token-plaintext-must-not-leak",
		"accessToken":           "camel-token-must-not-leak",
		"clientSecret":          "camel-secret-must-not-leak",
		"tenant":                "unneeded-routing-data",
	})
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	meta := requestExecutionMetadata(ctx)

	if got := meta[coreexecutor.AccessProviderMetadataKey]; got != "plugin:bravo:bravo" {
		t.Fatalf("AccessProviderMetadataKey = %#v", got)
	}
	accessMetadata, ok := meta[coreexecutor.AccessMetadataMetadataKey].(map[string]string)
	if !ok {
		t.Fatalf("AccessMetadataMetadataKey = %#v", meta[coreexecutor.AccessMetadataMetadataKey])
	}
	if accessMetadata["bravo_key_name"] != "primary" ||
		accessMetadata["bravo_project_id"] != "prj_primary" ||
		accessMetadata["bravo_allowed_models"] != "opus,sonnet" ||
		accessMetadata["bravo_access_provider"] != "bravo" {
		t.Fatalf("sanitized access metadata = %#v", accessMetadata)
	}
	for _, sensitive := range []string{"principal", "api_key", "access_token", "accessToken", "clientSecret", "tenant"} {
		if _, exists := accessMetadata[sensitive]; exists {
			t.Fatalf("non-allowlisted access metadata key %q leaked: %#v", sensitive, accessMetadata)
		}
	}
	for key, value := range meta {
		if key == "userApiKey" ||
			value == "sk-plaintext-must-not-leak" ||
			value == "token-plaintext-must-not-leak" ||
			value == "camel-token-must-not-leak" ||
			value == "camel-secret-must-not-leak" {
			t.Fatalf("plaintext credential leaked in execution metadata: %#v", meta)
		}
	}
}

func TestRequestExecutionMetadataDropsBravoFieldsFromOtherAccessProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Set("accessProvider", "config-inline")
	ginCtx.Set("accessMetadata", map[string]string{
		"bravo_access_provider": "bravo",
		"bravo_project_id":      "prj_spoofed",
	})
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	meta := requestExecutionMetadata(ctx)

	if got := meta[coreexecutor.AccessProviderMetadataKey]; got != "config-inline" {
		t.Fatalf("AccessProviderMetadataKey = %#v", got)
	}
	if _, exists := meta[coreexecutor.AccessMetadataMetadataKey]; exists {
		t.Fatalf("untrusted access metadata crossed into execution metadata: %#v", meta)
	}
}

func TestSanitizedAccessMetadataRejectsInvalidBravoConstraints(t *testing.T) {
	tooLongName := strings.Repeat("a", 121)
	tooLongModels := strings.Repeat("m", (32<<10)+1)
	got := sanitizedAccessMetadata(map[string]string{
		"bravo_access_provider": "not-bravo",
		"bravo_project_id":      "contains/slash",
		"bravo_key_name":        tooLongName,
		"bravo_allowed_models":  tooLongModels,
	})
	if len(got) != 0 {
		t.Fatalf("invalid Bravo metadata survived constraints: %#v", got)
	}
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataExtractsValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "priority")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "auto" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "auto")
	}
}

func TestSetServiceTierMetadataPreservesExplicitDefault(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"default"}`))

	if gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]; gotServiceTier != "default" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "default")
	}
}

func TestSetGenerateMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != true {
		t.Fatalf("GenerateMetadataKey = %v, want true", got)
	}
}

func TestSetGenerateMetadataPreservesTrue(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"generate":true}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != true {
		t.Fatalf("GenerateMetadataKey = %v, want true", got)
	}
}

func TestSetGenerateMetadataHonorsExplicitFalse(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"generate":false}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != false {
		t.Fatalf("GenerateMetadataKey = %v, want false", got)
	}
}
