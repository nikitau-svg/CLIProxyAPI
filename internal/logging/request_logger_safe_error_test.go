package logging

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestForcedErrorLogOmitsBodiesAndCredentialFragments(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	secret := "UNIQUE-SECRET-PROMPT-AND-TOKEN"

	errLog := logger.LogRequestWithOptions(
		"/v1/messages",
		http.MethodPost,
		map[string][]string{
			"Authorization": {"Bearer " + secret},
			"Content-Type":  {"application/json"},
		},
		[]byte(`{"messages":[{"content":"`+secret+`"}]}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
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
		"Authorization: Bearer [REDACTED]",
		"production error logs do not persist request bodies",
		"code=unclassified_request_failure",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("forced error log missing %q: %s", required, content)
		}
	}
}
