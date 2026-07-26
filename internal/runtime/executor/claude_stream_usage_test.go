package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const claudeStreamingUsageSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestClaudeExecutorExecutePublishesCombinedStreamingUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeStreamingUsageSSE))
	}))
	defer server.Close()

	const authID = "claude-execute-streaming-usage"
	plugin := &captureClaudeStreamingUsagePlugin{
		authID:  authID,
		records: make(chan usage.Record, 2),
	}
	usage.RegisterPlugin(plugin)

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	record := waitForClaudeStreamingUsageRecord(t, plugin.records)
	if record.Failed {
		t.Fatalf("usage record failed = true: %+v", record.Fail)
	}
	if record.Detail.InputTokens != 100 ||
		record.Detail.OutputTokens != 7 ||
		record.Detail.CachedTokens != 30 ||
		record.Detail.CacheReadTokens != 30 ||
		record.Detail.CacheCreationTokens != 20 ||
		record.Detail.TotalTokens != 157 {
		t.Fatalf("published Claude usage = %+v", record.Detail)
	}
	assertNoAdditionalClaudeStreamingUsageRecord(t, plugin.records)
}

func TestClaudeExecutorExecuteStreamPublishesCombinedUsage(t *testing.T) {
	tests := []struct {
		name         string
		sourceFormat sdktranslator.Format
		payload      []byte
	}{
		{
			name:         "claude passthrough",
			sourceFormat: sdktranslator.FormatClaude,
			payload:      []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		},
		{
			name:         "translated openai",
			sourceFormat: sdktranslator.FormatOpenAI,
			payload:      []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(claudeStreamingUsageSSE))
			}))
			defer server.Close()

			authID := "claude-stream-usage-" + tc.name
			plugin := &captureClaudeStreamingUsagePlugin{
				authID:  authID,
				records: make(chan usage.Record, 2),
			}
			usage.RegisterPlugin(plugin)

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				ID:       authID,
				Provider: "claude",
				Attributes: map[string]string{
					"api_key":  "key-123",
					"base_url": server.URL,
				},
			}
			result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-3-5-sonnet-20241022",
				Payload: tc.payload,
			}, cliproxyexecutor.Options{SourceFormat: tc.sourceFormat})
			if errExecute != nil {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v", chunk.Err)
				}
			}

			record := waitForClaudeStreamingUsageRecord(t, plugin.records)
			if record.Failed {
				t.Fatalf("usage record failed = true: %+v", record.Fail)
			}
			if record.Detail.InputTokens != 100 ||
				record.Detail.OutputTokens != 7 ||
				record.Detail.CachedTokens != 30 ||
				record.Detail.CacheReadTokens != 30 ||
				record.Detail.CacheCreationTokens != 20 ||
				record.Detail.TotalTokens != 157 {
				t.Fatalf("published Claude usage = %+v", record.Detail)
			}
			assertNoAdditionalClaudeStreamingUsageRecord(t, plugin.records)
		})
	}
}

type captureClaudeStreamingUsagePlugin struct {
	authID  string
	records chan usage.Record
}

func (p *captureClaudeStreamingUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p == nil || record.Provider != "claude" || record.AuthID != p.authID {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func waitForClaudeStreamingUsageRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Claude streaming usage record")
		return usage.Record{}
	}
}

func assertNoAdditionalClaudeStreamingUsageRecord(t *testing.T, records <-chan usage.Record) {
	t.Helper()
	select {
	case record := <-records:
		t.Fatalf("received additional Claude streaming usage record: %+v", record)
	case <-time.After(100 * time.Millisecond):
	}
}
