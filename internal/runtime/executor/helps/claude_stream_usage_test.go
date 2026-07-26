package helps

import "testing"

func TestStreamUsageBufferCombinesClaudeMessageStartAndDelta(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveClaudeStream([]byte(
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}}}`,
	))
	buffer.ObserveClaudeStream([]byte(
		`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
	))

	detail, ok := buffer.Detail()
	if !ok {
		t.Fatal("Detail() ok = false, want true")
	}
	if detail.InputTokens != 100 ||
		detail.OutputTokens != 7 ||
		detail.CachedTokens != 30 ||
		detail.CacheReadTokens != 30 ||
		detail.CacheCreationTokens != 20 ||
		detail.TotalTokens != 157 {
		t.Fatalf("combined Claude usage = %+v", detail)
	}
}
