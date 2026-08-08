package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

func executionBody(req rpcExecutorRequest) []byte {
	return bytes.Clone(executionBodyView(req))
}

func executionBodyView(req rpcExecutorRequest) []byte {
	if len(req.OriginalRequest) > 0 {
		return req.OriginalRequest
	}
	return req.Payload
}

func candidateModelName(item candidate) string {
	model := strings.TrimSpace(item.Model)
	effort := normalizeEffort(item.Effort)
	if effort == "" || effort == "none" || effort == "auto" {
		return model
	}
	return fmt.Sprintf("%s(%s)", model, effort)
}

func effectiveCandidateEffort(item candidate, requested requestEffort) string {
	if requested.Specified && requested.Value != "auto" {
		return normalizeEffort(requested.Value)
	}
	return normalizeEffort(item.Effort)
}

func effortEnablesThinking(effort string) bool {
	switch normalizeEffort(effort) {
	case "", "auto", "none":
		return false
	default:
		return true
	}
}

func rewriteRequestModel(body []byte, physicalModel string, stream bool) ([]byte, error) {
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil, fmt.Errorf("decode client request: %w", errUnmarshal)
	}
	if root == nil {
		return nil, fmt.Errorf("client request must be a JSON object")
	}
	root["model"] = physicalModel
	if stream {
		root["stream"] = true
	}
	return json.Marshal(root)
}

func rewriteCandidateRequest(body []byte, protocol, physicalModel string, stream bool, contentType ...string) ([]byte, error) {
	if normalizeContractProtocol(protocol) == protocolOpenAIImage {
		requestContentType := ""
		if len(contentType) > 0 {
			requestContentType = strings.TrimSpace(contentType[0])
		}
		if strings.HasPrefix(strings.ToLower(requestContentType), "multipart/form-data") {
			return rewriteMultipartRequestModel(body, requestContentType, physicalModel, stream)
		}
	}

	rewritten, errRewrite := rewriteRequestModel(body, physicalModel, stream)
	if errRewrite != nil {
		return nil, errRewrite
	}
	if normalizeContractProtocol(protocol) != protocolOpenAIResponse {
		return rewritten, nil
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(rewritten, &root); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if input, ok := root["input"].(string); ok {
		root["input"] = []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": input,
			}},
		}}
	}
	return json.Marshal(root)
}

func rewriteMultipartRequestModel(body []byte, contentType, physicalModel string, stream bool) ([]byte, error) {
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		return nil, fmt.Errorf("decode multipart content type: %w", errParse)
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") {
		return nil, fmt.Errorf("unsupported image request content type %q", mediaType)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, fmt.Errorf("multipart image request is missing a boundary")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var rewritten bytes.Buffer
	writer := multipart.NewWriter(&rewritten)
	if errBoundary := writer.SetBoundary(boundary); errBoundary != nil {
		return nil, fmt.Errorf("preserve multipart boundary: %w", errBoundary)
	}

	modelWritten := false
	streamWritten := false
	for {
		part, errNext := reader.NextPart()
		if errNext == io.EOF {
			break
		}
		if errNext != nil {
			return nil, fmt.Errorf("decode multipart image request: %w", errNext)
		}
		formName := part.FormName()
		if formName == "stream" && !stream {
			continue
		}

		target, errCreate := writer.CreatePart(part.Header)
		if errCreate != nil {
			return nil, fmt.Errorf("copy multipart image field %q: %w", formName, errCreate)
		}
		switch formName {
		case "model":
			if _, errWrite := io.WriteString(target, physicalModel); errWrite != nil {
				return nil, fmt.Errorf("rewrite multipart image model: %w", errWrite)
			}
			modelWritten = true
		case "stream":
			if _, errWrite := io.WriteString(target, "true"); errWrite != nil {
				return nil, fmt.Errorf("rewrite multipart image stream flag: %w", errWrite)
			}
			streamWritten = true
		default:
			if _, errCopy := io.Copy(target, part); errCopy != nil {
				return nil, fmt.Errorf("copy multipart image field %q: %w", formName, errCopy)
			}
		}
	}
	if !modelWritten {
		if errWrite := writer.WriteField("model", physicalModel); errWrite != nil {
			return nil, fmt.Errorf("add multipart image model: %w", errWrite)
		}
	}
	if stream && !streamWritten {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, fmt.Errorf("add multipart image stream flag: %w", errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, fmt.Errorf("finish multipart image request: %w", errClose)
	}
	return rewritten.Bytes(), nil
}

func rewriteResponseModel(body []byte, physicalModel, logicalModel string) []byte {
	var value any
	if errUnmarshal := json.Unmarshal(body, &value); errUnmarshal != nil {
		return replaceModelLiterals(body, physicalModel, logicalModel)
	}
	rewriteModelFields(value, physicalModel, logicalModel)
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return replaceModelLiterals(body, physicalModel, logicalModel)
	}
	return raw
}

func rewriteModelFields(value any, physicalModel, logicalModel string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "model") {
				if model, ok := child.(string); ok && modelMatchesPhysical(model, physicalModel) {
					typed[key] = logicalModel
					continue
				}
			}
			rewriteModelFields(child, physicalModel, logicalModel)
		}
	case []any:
		for _, child := range typed {
			rewriteModelFields(child, physicalModel, logicalModel)
		}
	}
}

func modelMatchesPhysical(value, physicalModel string) bool {
	value = strings.TrimSpace(value)
	physicalModel = strings.TrimSpace(physicalModel)
	if value == physicalModel {
		return true
	}
	if open := strings.LastIndex(physicalModel, "("); open > 0 && strings.HasSuffix(physicalModel, ")") {
		return value == physicalModel[:open]
	}
	return false
}

func replaceModelLiterals(body []byte, physicalModel, logicalModel string) []byte {
	out := bytes.Clone(body)
	physicalBase := physicalModel
	if open := strings.LastIndex(physicalBase, "("); open > 0 && strings.HasSuffix(physicalBase, ")") {
		physicalBase = physicalBase[:open]
	}
	for _, physical := range []string{physicalModel, physicalBase} {
		if physical == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(`"`+physical+`"`), []byte(`"`+logicalModel+`"`))
	}
	return out
}

type streamModelRewriter struct {
	physical string
	logical  string
	protocol string
	pending  []byte
}

func (r *streamModelRewriter) Push(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	// Native model streams expose protocol-specific chunk contracts:
	//
	//   - OpenAI Chat emits one raw JSON object per chunk. The HTTP handler
	//     adds the `data:` wrapper and event delimiter.
	//   - Responses and Claude emit SSE event bytes. A complete event is often
	//     delivered without a trailing blank line; their HTTP handlers frame
	//     it on output.
	//
	// Buffering all three until "\n\n" corrupts Chat into `{...}{...}` and
	// joins Responses events as `}event:`. Preserve each native chunk boundary
	// for known protocols and only retain the incremental framer as a defensive
	// fallback for callers that do not declare a protocol.
	switch normalizeContractProtocol(r.protocol) {
	case protocolOpenAI:
		return [][]byte{rewriteResponseModel(payload, r.physical, r.logical)}
	case protocolOpenAIResponse, protocolClaude, protocolOpenAIImage:
		return [][]byte{rewriteSSEFrameModel(payload, r.physical, r.logical)}
	}
	r.pending = append(r.pending, payload...)
	var out [][]byte
	for {
		index, width := nextSSEBoundary(r.pending)
		if index < 0 {
			break
		}
		end := index + width
		frame := bytes.Clone(r.pending[:end])
		r.pending = append(r.pending[:0], r.pending[end:]...)
		out = append(out, rewriteSSEFrameModel(frame, r.physical, r.logical))
	}
	if len(r.pending) > 2<<20 {
		out = append(out, replaceModelLiterals(r.pending, r.physical, r.logical))
		r.pending = nil
	}
	return out
}

func (r *streamModelRewriter) Flush() []byte {
	if normalizeContractProtocol(r.protocol) != "" {
		return nil
	}
	if len(r.pending) == 0 {
		return nil
	}
	out := rewriteSSEFrameModel(r.pending, r.physical, r.logical)
	r.pending = nil
	return out
}

func nextSSEBoundary(payload []byte) (int, int) {
	lf := bytes.Index(payload, []byte("\n\n"))
	crlf := bytes.Index(payload, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	case crlf < 0:
		return lf, 2
	case crlf < lf:
		return crlf, 4
	default:
		return lf, 2
	}
}

func rewriteSSEFrameModel(frame []byte, physicalModel, logicalModel string) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	for index, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var value any
		if errUnmarshal := json.Unmarshal(data, &value); errUnmarshal != nil {
			lines[index] = replaceModelLiterals(line, physicalModel, logicalModel)
			continue
		}
		rewriteModelFields(value, physicalModel, logicalModel)
		rewritten, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			continue
		}
		lineEnding := []byte("\n")
		if bytes.HasSuffix(line, []byte("\r\n")) {
			lineEnding = []byte("\r\n")
		} else if !bytes.HasSuffix(line, []byte("\n")) {
			lineEnding = nil
		}
		lines[index] = append(append([]byte("data: "), rewritten...), lineEnding...)
	}
	return bytes.Join(lines, nil)
}

func sanitizedNestedHeaders(source http.Header) http.Header {
	out := cloneHeader(source)
	if out == nil {
		out = make(http.Header)
	}
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Api-Key",
		"X-Goog-Api-Key",
		"Cookie",
		"Content-Length",
		"Connection",
	} {
		out.Del(name)
	}
	if strings.TrimSpace(out.Get("Content-Type")) == "" {
		out.Set("Content-Type", "application/json")
	}
	return out
}

func sanitizedNestedQuery(source url.Values) url.Values {
	out := make(url.Values, len(source))
	for key, values := range source {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "key", "auth_token":
			continue
		default:
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}
