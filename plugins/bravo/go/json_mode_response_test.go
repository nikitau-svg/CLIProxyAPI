package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizeOpenAIChatJSONModeResponse(t *testing.T) {
	t.Parallel()

	jsonModeRequest := []byte(`{"response_format":{"type":"json_object"}}`)
	plainRequest := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	tests := []struct {
		name    string
		request []byte
		content string
		want    string
	}{
		{name: "json fence", request: jsonModeRequest, content: "```json\n{\"results\":[]}\n```", want: `{"results":[]}`},
		{name: "unlabelled fence", request: jsonModeRequest, content: "```\n {\"ok\": true} \n```", want: `{"ok": true}`},
		{name: "bare object", request: jsonModeRequest, content: `{"ok":true}`, want: `{"ok":true}`},
		{name: "array stays fenced", request: jsonModeRequest, content: "```json\n[]\n```", want: "```json\n[]\n```"},
		{name: "prose stays", request: jsonModeRequest, content: "answer: {\"ok\":true}", want: "answer: {\"ok\":true}"},
		{name: "wrong fence stays", request: jsonModeRequest, content: "```javascript\n{\"ok\":true}\n```", want: "```javascript\n{\"ok\":true}\n```"},
		{name: "malformed stays", request: jsonModeRequest, content: "```json\n{\"ok\":\n```", want: "```json\n{\"ok\":\n```"},
		{name: "ordinary request stays fenced", request: plainRequest, content: "```json\n{\"ok\":true}\n```", want: "```json\n{\"ok\":true}\n```"},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			body, errMarshal := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "content": testCase.content},
				}},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			normalized := normalizeOpenAIChatJSONModeResponse(body, testCase.request, protocolOpenAI)
			var response struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if errUnmarshal := json.Unmarshal(normalized, &response); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if got := response.Choices[0].Message.Content; got != testCase.want {
				t.Fatalf("content = %q, want %q", got, testCase.want)
			}
		})
	}
}
