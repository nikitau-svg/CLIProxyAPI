// Package providererror extracts safe, structured details from provider error
// response bodies.
package providererror

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const (
	creditsRequiredCode          = "credits_required"
	monthlySpendLimitNoticeTitle = "You've hit your monthly spend limit"
	rateLimitErrorType           = "rate_limit_error"
	maxProviderErrorPayloadBytes = 256 << 10
)

// Detail contains the provider error fields that are safe to propagate across
// executor and plugin boundaries.
type Detail struct {
	Type             string `json:"type,omitempty"`
	Code             string `json:"code,omitempty"`
	Message          string `json:"message,omitempty"`
	Model            string `json:"model,omitempty"`
	ModelDisplayName string `json:"model_display_name,omitempty"`
	NoticeTitle      string `json:"notice_title,omitempty"`
	NoticeText       string `json:"notice_text,omitempty"`
	DisabledReason   string `json:"disabled_reason,omitempty"`
	Scope            string `json:"scope,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

// DetailProvider exposes an already-sanitized provider diagnostic without
// forcing callers to retain or reparse the raw upstream response.
type DetailProvider interface {
	ProviderErrorDetail() (Detail, bool)
}

// Classification contains the reviewed routing semantics of a documented
// provider error. Detail is safe to cross executor, plugin, analytics, and
// persistence boundaries; it never retains the raw provider response.
type Classification struct {
	Detail    Detail `json:"detail"`
	Status    int    `json:"status"`
	Retryable bool   `json:"retryable"`
}

// FromError extracts and sanitizes a provider diagnostic carried by an error.
// Empty details are rejected so unknown errors cannot accidentally become a
// reviewed provider signal at a later boundary.
func FromError(err error) (Detail, bool) {
	if err == nil {
		return Detail{}, false
	}
	var provider DetailProvider
	if !errors.As(err, &provider) || provider == nil {
		return Detail{}, false
	}
	detail, ok := provider.ProviderErrorDetail()
	if !ok {
		return Detail{}, false
	}
	detail = Sanitize(detail)
	if detail.Code == "" && detail.Type == "" && detail.Message == "" &&
		detail.Model == "" && detail.ModelDisplayName == "" &&
		detail.NoticeTitle == "" && detail.NoticeText == "" &&
		detail.DisabledReason == "" && detail.Scope == "" && detail.Reason == "" {
		return Detail{}, false
	}
	return detail, true
}

type providerErrorEnvelope struct {
	Type  string            `json:"type"`
	Error providerErrorBody `json:"error"`
}

type providerErrorBody struct {
	Type    string               `json:"type"`
	Message string               `json:"message"`
	Details providerErrorDetails `json:"details"`
}

type providerErrorDetails struct {
	ErrorCode        string              `json:"error_code"`
	Model            string              `json:"model"`
	ModelDisplayName string              `json:"model_display_name"`
	Notice           providerErrorNotice `json:"notice"`
	DisabledReason   string              `json:"disabled_reason"`
}

type providerErrorNotice struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

// Parse extracts a provider error from a JSON response body. A short status-code
// prefix before the JSON object is accepted because some executor boundaries
// wrap upstream bodies that way. Unknown fields are deliberately discarded.
func Parse(value string) (Detail, bool) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > maxProviderErrorPayloadBytes {
		return Detail{}, false
	}
	objectStart := strings.IndexByte(value, '{')
	if objectStart < 0 {
		return Detail{}, false
	}

	var envelope providerErrorEnvelope
	if err := json.Unmarshal([]byte(value[objectStart:]), &envelope); err != nil {
		return Detail{}, false
	}
	if strings.TrimSpace(envelope.Type) != "error" {
		return Detail{}, false
	}

	if strings.TrimSpace(envelope.Error.Type) != rateLimitErrorType ||
		strings.TrimSpace(envelope.Error.Details.ErrorCode) != creditsRequiredCode {
		return Detail{}, false
	}
	detail := Sanitize(Detail{
		Type:             rateLimitErrorType,
		Code:             creditsRequiredCode,
		Message:          envelope.Error.Message,
		Model:            envelope.Error.Details.Model,
		ModelDisplayName: envelope.Error.Details.ModelDisplayName,
		NoticeTitle:      envelope.Error.Details.Notice.Title,
		NoticeText:       envelope.Error.Details.Notice.Text,
		DisabledReason:   envelope.Error.Details.DisabledReason,
	})
	if detail.Model != "" {
		detail.Scope = "model"
	}
	if detail.NoticeTitle == monthlySpendLimitNoticeTitle {
		detail.Reason = "monthly_spend_limit"
	}
	return detail, true
}

// ParseAnthropicStandard recognizes the documented top-level Anthropic error
// envelope without retaining provider-authored diagnostics. The special
// credits_required contract remains handled by Parse so existing persisted
// quota behavior is unchanged.
func ParseAnthropicStandard(value string) (Classification, bool) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > maxProviderErrorPayloadBytes {
		return Classification{}, false
	}

	var envelope providerErrorEnvelope
	if err := json.Unmarshal([]byte(value), &envelope); err != nil {
		return Classification{}, false
	}
	if strings.TrimSpace(envelope.Type) != "error" {
		return Classification{}, false
	}

	errorType := strings.TrimSpace(envelope.Error.Type)
	status, retryable, scope, message, ok := anthropicStandardClassification(errorType)
	if !ok {
		return Classification{}, false
	}
	detail := Sanitize(Detail{
		Type:    errorType,
		Code:    errorType,
		Message: message,
		Scope:   scope,
	})
	return Classification{
		Detail:    detail,
		Status:    status,
		Retryable: retryable,
	}, true
}

func anthropicStandardClassification(errorType string) (
	status int,
	retryable bool,
	scope string,
	message string,
	ok bool,
) {
	switch errorType {
	case "invalid_request_error":
		return http.StatusBadRequest, false, "request", "The provider rejected the request.", true
	case "authentication_error":
		return http.StatusUnauthorized, true, "account", "The provider rejected the subscription credentials.", true
	case "billing_error":
		return http.StatusPaymentRequired, true, "account", "The provider reported a billing restriction.", true
	case "permission_error":
		return http.StatusForbidden, true, "account", "The provider denied this subscription access.", true
	case "not_found_error":
		return http.StatusNotFound, false, "request", "The provider could not find the requested resource.", true
	case "conflict_error":
		return http.StatusConflict, false, "request", "The request conflicts with provider state.", true
	case "request_too_large":
		return http.StatusRequestEntityTooLarge, false, "request", "The request exceeds the provider size limit.", true
	case "rate_limit_error":
		return http.StatusTooManyRequests, true, "model", "The provider rate limit was reached.", true
	case "api_error":
		return http.StatusInternalServerError, true, "model", "The provider encountered an internal error.", true
	case "timeout_error":
		return http.StatusGatewayTimeout, true, "model", "The provider timed out while processing the request.", true
	case "overloaded_error":
		return 529, true, "model", "The provider is temporarily overloaded.", true
	default:
		return 0, false, "", "", false
	}
}

// Sanitize bounds and redacts every provider-authored field in a Detail. It is
// safe to apply repeatedly at host and plugin boundaries.
func Sanitize(detail Detail) Detail {
	return Detail{
		Type:             safeMachineText(detail.Type, 64),
		Code:             safeMachineText(detail.Code, 128),
		Message:          safeProviderText(detail.Message, 512),
		Model:            safeMachineText(detail.Model, 256),
		ModelDisplayName: safeProviderText(detail.ModelDisplayName, 160),
		NoticeTitle:      safeProviderText(detail.NoticeTitle, 240),
		NoticeText:       safeProviderText(detail.NoticeText, 600),
		DisabledReason:   safeMachineText(detail.DisabledReason, 128),
		Scope:            safeMachineText(detail.Scope, 32),
		Reason:           safeMachineText(detail.Reason, 128),
	}
}

// Summary returns a concise, single-line description without retaining the raw
// provider response body.
func (d Detail) Summary() string {
	d = Sanitize(d)
	summary := firstNonEmpty(d.NoticeTitle, d.Message, d.Code, d.Type)
	if summary == "" {
		return ""
	}

	model := firstNonEmpty(d.ModelDisplayName, d.Model)
	if model == "" || strings.Contains(strings.ToLower(summary), strings.ToLower(model)) {
		return summary
	}
	return model + ": " + summary
}

func safeProviderText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" || sensitiveProviderText(value) {
		return ""
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes])
	}
	return value
}

func safeMachineText(value string, maxRunes int) string {
	value = safeProviderText(value, maxRunes)
	if value == "" {
		return ""
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_',
			char == '-',
			char == '.',
			char == ':',
			char == '/',
			char == '(',
			char == ')':
		default:
			return ""
		}
	}
	return value
}

func sensitiveProviderText(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"request_id",
		"request-id",
		"request id",
		"authorization",
		"bearer ",
		"api_key",
		"api-key",
		"api key",
		"access_token",
		"access-token",
		"refresh_token",
		"refresh-token",
		"session_key",
		"session-key",
		"payment_method",
		"payment-method",
		"credit card",
		"cookie:",
		"set-cookie",
		"sk-",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return sensitiveCredentialMarker(lower)
}

func sensitiveCredentialMarker(value string) bool {
	words := strings.FieldsFunc(value, func(char rune) bool {
		return !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9'))
	})
	for index := range words {
		combined := ""
		for end := index; end < len(words) && end < index+3; end++ {
			combined += words[end]
			switch combined {
			case "requestid",
				"apikey",
				"accesstoken",
				"refreshtoken",
				"sessionkey",
				"sessiontoken",
				"paymentmethod",
				"clientsecret",
				"idtoken",
				"privatekey",
				"secretkey",
				"secretaccesskey",
				"password",
				"passphrase":
				return true
			}
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
