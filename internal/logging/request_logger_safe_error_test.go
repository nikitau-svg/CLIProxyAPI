package logging

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/providererror"
)

type providerDetailErrorForSafeLog struct {
	detail providererror.Detail
	code   string
}

func (e providerDetailErrorForSafeLog) Error() string { return e.detail.Message }

func (e providerDetailErrorForSafeLog) ErrorCode() string { return e.code }

func (e providerDetailErrorForSafeLog) ProviderErrorDetail() (providererror.Detail, bool) {
	return e.detail, true
}

func TestForcedErrorLogOmitsBodiesAndCredentialFragments(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	secret := "UNIQUE-SECRET-PROMPT-AND-TOKEN"

	errLog := logger.LogRequestWithOptions(
		"/v1/messages?key="+secret+"&q="+secret,
		http.MethodPost,
		map[string][]string{
			"Authorization": {"Bearer " + secret},
			"Content-Type":  {"application/json"},
			"X-Credential":  {secret},
		},
		[]byte(`{"messages":[{"content":"`+secret+`"}]}`),
		http.StatusBadGateway,
		map[string][]string{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + secret},
			"Set-Cookie":    {"session=" + secret},
			"X-Api-Key":     {secret},
			"X-Credential":  {secret},
		},
		[]byte(`{"error":"`+secret+`"}`),
		nil,
		[]byte("upstream request "+secret),
		[]byte("upstream response "+secret),
		nil,
		[]*interfaces.ErrorMessage{{StatusCode: http.StatusBadGateway, Error: errors.New(secret)}},
		true,
		"safe-error-log",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errRead)
	}
	raw, errRead := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errRead != nil {
		t.Fatalf("read error log: %v", errRead)
	}
	content := string(raw)
	if strings.Contains(content, secret) || strings.Contains(content, "UNIQUE") {
		t.Fatalf("forced error log leaked request data: %s", content)
	}
	for _, required := range []string{
		"URL: /v1/messages?[REDACTED]",
		"Authorization: [REDACTED]",
		"Set-Cookie: [REDACTED]",
		"X-Api-Key: [REDACTED]",
		"X-Credential: [REDACTED]",
		"Content-Type: application/json",
		"production error logs do not persist request bodies",
		"code=http_502_failure",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("forced error log missing %q: %s", required, content)
		}
	}
	info, errStat := os.Stat(filepath.Join(logsDir, entries[0].Name()))
	if errStat != nil {
		t.Fatalf("stat error log: %v", errStat)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("error log mode = %04o, want 0600", got)
	}
}

func TestForcedErrorLogKeepsOnlyValidatedRequestBodyMetadata(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	metadata := FormatErrorRequestBodyMetadata(ErrorRequestBodyMetadata{
		DeclaredBytes: 200,
		ConsumedBytes: 17,
		Complete:      false,
	})

	errLog := logger.LogRequestWithOptions(
		"/v1/messages",
		http.MethodPost,
		nil,
		metadata,
		http.StatusBadRequest,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"metadata-error-log",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errRead)
	}
	raw, errRead := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errRead != nil {
		t.Fatalf("read error log: %v", errRead)
	}
	content := string(raw)
	for _, want := range []string{
		errorRequestBodyOmitted,
		`[REQUEST BODY METADATA] {"declared_bytes":200,"consumed_bytes":17,"complete":false}`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("error log missing %q: %s", want, content)
		}
	}
}

func TestForcedErrorLogKeepsOnlyValidatedResponseBodyMetadata(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	metadata := FormatErrorResponseBodyMetadata(ErrorResponseBodyMetadata{WrittenBytes: 321})

	errLog := logger.LogRequestWithOptions(
		"/v1/messages", http.MethodPost, nil, nil, http.StatusBadRequest,
		nil, metadata, nil, nil, nil, nil, nil, true, "response-metadata", time.Now(), time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions: %v", errLog)
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errRead)
	}
	raw, errRead := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errRead != nil {
		t.Fatalf("read error log: %v", errRead)
	}
	for _, want := range []string{
		errorResponseBodyOmitted,
		`[RESPONSE BODY METADATA] {"written_bytes":321}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("error log missing %q: %s", want, raw)
		}
	}
}

func TestForcedErrorLogRejectsForgedRequestBodyMetadata(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	secret := "MUST-NOT-BE-WRITTEN"
	forged := []byte(errorRequestBodyMetadataPrefix + `{"declared_bytes":1,"consumed_bytes":1,"complete":true,"body":"` + secret + `"}`)

	errLog := logger.LogRequestWithOptions(
		"/v1/messages", http.MethodPost, nil, forged, http.StatusBadRequest,
		nil, nil, nil, nil, nil, nil, nil, true, "forged-metadata", time.Now(), time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions: %v", errLog)
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errRead)
	}
	raw, errRead := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errRead != nil {
		t.Fatalf("read error log: %v", errRead)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), errorRequestBodyMetadataPrefix) {
		t.Fatalf("forged metadata escaped sanitization: %s", raw)
	}
}

func TestForcedErrorLogOmitsProviderAuthoredMessage(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	secret := "PROMPT-CONTENT-QUOTED-BY-PROVIDER"
	errProvider := providerDetailErrorForSafeLog{detail: providererror.Detail{
		Type:            secret,
		Code:            secret,
		Message:         "provider rejected: " + secret,
		Scope:           providererror.ScopeAccount,
		Reason:          secret,
		TaxonomyVersion: providererror.FailureTaxonomyV1,
		Class:           providererror.ClassQuota,
	}, code: secret}

	errLog := logger.LogRequestWithOptions(
		"/v1/messages", http.MethodPost, nil, nil, http.StatusTooManyRequests,
		nil, nil, nil, nil, nil, nil,
		[]*interfaces.ErrorMessage{{StatusCode: http.StatusTooManyRequests, Error: errProvider}},
		true, "provider-message-safe-log", time.Now(), time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions: %v", errLog)
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errRead)
	}
	raw, errRead := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errRead != nil {
		t.Fatalf("read error log: %v", errRead)
	}
	content := string(raw)
	if strings.Contains(content, secret) || strings.Contains(content, "provider rejected") {
		t.Fatalf("forced error log retained provider-authored message: %s", content)
	}
	for _, want := range []string{"code=provider_quota", "type=provider_error", "scope=account", "reason=quota", "class=quota"} {
		if !strings.Contains(content, want) {
			t.Fatalf("forced error log missing closed diagnostic %q: %s", want, content)
		}
	}
}

func TestForcedErrorLogPreservesOnlyRegisteredBravoCodes(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	secret := "UNREGISTERED-BRAVO-SECRET"
	for index, code := range []string{"bravo_no_eligible_account", "bravo_" + secret} {
		errLog := logger.LogRequestWithOptions(
			"/v1/messages", http.MethodPost, nil, nil, http.StatusServiceUnavailable,
			nil, nil, nil, nil, nil, nil,
			[]*interfaces.ErrorMessage{{
				StatusCode:       http.StatusServiceUnavailable,
				Error:            providerDetailErrorForSafeLog{code: code},
				ExecutorPluginID: "bravo",
			}},
			true, "registered-bravo-"+strconv.Itoa(index), time.Now(), time.Now(),
		)
		if errLog != nil {
			t.Fatalf("LogRequestWithOptions(%q): %v", code, errLog)
		}
	}
	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil || len(entries) != 2 {
		t.Fatalf("error log entries = %v, err=%v", entries, errRead)
	}
	var all strings.Builder
	for _, entry := range entries {
		raw, errFile := os.ReadFile(filepath.Join(logsDir, entry.Name()))
		if errFile != nil {
			t.Fatal(errFile)
		}
		all.Write(raw)
	}
	content := all.String()
	if strings.Contains(content, secret) {
		t.Fatalf("unregistered Bravo code leaked provider-authored text: %s", content)
	}
	for _, want := range []string{"code=bravo_no_eligible_account", "code=bravo_failure"} {
		if !strings.Contains(content, want) {
			t.Fatalf("forced error logs missing %q: %s", want, content)
		}
	}
}
