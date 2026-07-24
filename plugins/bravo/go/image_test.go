package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecuteStreamRejectsUnverifiedImageContractSynchronously(t *testing.T) {
	previousConfig := loadedConfig()
	cfg := pluginConfig{
		Enabled:         true,
		Prefix:          defaultPrefix,
		RequireSmartKey: false,
		CooldownSeconds: 30,
		Models: map[string]logicalModel{
			"image-probe": {
				Candidates: []candidate{{
					Provider:     "codex",
					Model:        "gpt-image-2",
					Capabilities: []string{capabilityImageGeneration},
				}},
			},
		},
	}
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatal(errNormalize)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
	})

	hostCalls := 0
	previousHostCall := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		hostCalls++
		return nil, nil
	}
	t.Cleanup(func() {
		hostCall = previousHostCall
	})

	request, errMarshal := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "bravo/image-probe",
			Format:          protocolOpenAIImage,
			SourceFormat:    protocolOpenAIImage,
			OriginalRequest: []byte(`{"model":"bravo/image-probe","prompt":"paint","stream":true}`),
		},
		StreamID: "image-probe-stream",
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errExecute := executeStream(request)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var result envelope
	if errUnmarshal := json.Unmarshal(raw, &result); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if result.OK || result.Error == nil {
		t.Fatalf("executeStream() = %#v, want synchronous contract error", result)
	}
	if result.Error.Code != "bravo_capability_undeclared" || result.Error.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("stream contract error = %#v", result.Error)
	}
	if hostCalls != 0 {
		t.Fatalf("host callbacks = %d, want zero before rejected stream", hostCalls)
	}
}

func TestDefaultConfigIncludesImageOnlyCodexAliases(t *testing.T) {
	t.Parallel()

	cfg := defaultPluginConfig()
	if errNormalize := normalizeConfig(&cfg); errNormalize != nil {
		t.Fatalf("normalizeConfig() error = %v", errNormalize)
	}

	for _, name := range []string{"image", "gpt-image-2", "gpt-image-1.5"} {
		model, ok := cfg.Models[name]
		if !ok {
			t.Fatalf("image alias %q is missing", name)
		}
		if !logicalModelIsImageOnly(model) {
			t.Fatalf("image alias %q is not classified as image-only", name)
		}
		for _, modelCandidate := range model.Candidates {
			if modelCandidate.Provider != "codex" {
				t.Fatalf("image alias %q contains non-Codex fallback %#v", name, modelCandidate)
			}
			capabilities := newCapabilitySet(modelCandidate.Capabilities...)
			if _, okImage := capabilities[capabilityImageGeneration]; !okImage {
				t.Fatalf("image alias %q candidate does not declare image_generation", name)
			}
			if _, hasText := capabilities[capabilityText]; hasText {
				t.Fatalf("image alias %q candidate unexpectedly declares text", name)
			}
		}
	}
	if got := cfg.Models["gpt-image-2"].Candidates[0].Model; got != "gpt-image-2" {
		t.Fatalf("gpt-image-2 primary candidate = %q", got)
	}
	if got := cfg.Models["gpt-image-1.5"].Candidates[0].Model; got != "gpt-image-1.5" {
		t.Fatalf("gpt-image-1.5 primary candidate = %q", got)
	}
}

func TestRegisteredImageModelMetadata(t *testing.T) {
	t.Parallel()

	cfg := defaultPluginConfig()
	model := registeredLogicalModel(defaultPrefix, "image", cfg.Models["image"])
	if model.Type != "openai-image" {
		t.Fatalf("model type = %q, want openai-image", model.Type)
	}
	if model.Thinking != nil {
		t.Fatalf("image model thinking = %#v, want nil", model.Thinking)
	}
	if !equalStrings(model.SupportedInputModalities, []string{"text", "image"}) {
		t.Fatalf("input modalities = %v", model.SupportedInputModalities)
	}
	if !equalStrings(model.SupportedOutputModalities, []string{"image"}) {
		t.Fatalf("output modalities = %v", model.SupportedOutputModalities)
	}
	if !containsString(model.SupportedGenerationMethods, "images.generate") ||
		!containsString(model.SupportedGenerationMethods, "images.edit") {
		t.Fatalf("generation methods = %v", model.SupportedGenerationMethods)
	}
	if containsString(model.SupportedGenerationMethods, "images.stream") {
		t.Fatalf("unverified stream method was advertised: %v", model.SupportedGenerationMethods)
	}
}

func TestPluginRegistrationDeclaresOpenAIImageExecutorFormat(t *testing.T) {
	t.Parallel()

	registration := pluginRegistration()
	if !containsString(registration.Capabilities.ExecutorInputFormats, protocolOpenAIImage) {
		t.Fatalf("executor input formats = %v", registration.Capabilities.ExecutorInputFormats)
	}
	if !containsString(registration.Capabilities.ExecutorOutputFormats, protocolOpenAIImage) {
		t.Fatalf("executor output formats = %v", registration.Capabilities.ExecutorOutputFormats)
	}
}

func TestDetectOpenAIImageContract(t *testing.T) {
	t.Parallel()

	nonStream, errDetect := detectRequestContract(
		protocolOpenAIImage,
		[]byte(`{"model":"bravo/image","prompt":"paint a red fox"}`),
		false,
	)
	if errDetect != nil {
		t.Fatalf("detectRequestContract() error = %v", errDetect)
	}
	assertCapabilities(t, nonStream, capabilityImageGeneration)

	stream, errDetect := detectRequestContract(
		protocolOpenAIImage,
		[]byte(`{"model":"bravo/image","prompt":"paint a red fox","stream":true}`),
		false,
	)
	if errDetect != nil {
		t.Fatalf("detectRequestContract(stream) error = %v", errDetect)
	}
	assertCapabilities(t, stream, capabilityImageGeneration, capabilityStream)

	multipartContract, errDetect := detectRequestContract(
		protocolOpenAIImage,
		[]byte("--BravoBoundary\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\npaint\r\n--BravoBoundary--\r\n"),
		true,
	)
	if errDetect != nil {
		t.Fatalf("detectRequestContract(multipart) error = %v", errDetect)
	}
	assertCapabilities(t, multipartContract, capabilityImageGeneration, capabilityStream)
}

func TestOpenAIImageContractAllowsLiveNonStreamAndRejectsStream(t *testing.T) {
	t.Parallel()

	item := candidate{
		Provider:     "codex",
		Model:        "gpt-image-2",
		Capabilities: []string{capabilityImageGeneration},
	}
	_, errPreflight := preflightCandidateContract(
		item,
		protocolOpenAIImage,
		[]byte(`{"model":"bravo/image","prompt":"paint a red fox"}`),
		false,
	)
	if errPreflight != nil {
		t.Fatalf("live-verified non-stream image contract failed: %v", errPreflight)
	}
	_, errPreflight = preflightCandidateContract(
		item,
		protocolOpenAIImage,
		[]byte(`{"model":"bravo/image","prompt":"paint a red fox","stream":true}`),
		true,
	)
	assertContractError(t, errPreflight, "bravo_capability_undeclared", capabilityStream)
	errPreflight = preflightLogicalModelContract(
		logicalModel{Candidates: []candidate{item}},
		protocolOpenAIImage,
		[]byte(`{"model":"bravo/image","prompt":"paint a red fox","stream":true}`),
		true,
	)
	assertContractError(t, errPreflight, "bravo_capability_undeclared", capabilityStream)
}

func TestNestedOpenAIImageRequestPinsAuthAndAllowsImageModel(t *testing.T) {
	t.Parallel()

	req := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Headers: http.Header{
				"Authorization": []string{"Bearer brv_secret"},
				"Content-Type":  []string{"multipart/form-data; boundary=BravoBoundary"},
			},
		},
	}
	attempt := executionAttempt{
		Candidate: candidate{Provider: "codex", Model: "gpt-image-2"},
		Auth:      pluginapi.HostAuthFileEntry{ID: "codex-account-1"},
	}
	nested := nestedHostModelRequest(req, attempt, protocolOpenAIImage, "gpt-image-2", []byte("body"), true)
	if !nested.AllowImageModel || !nested.SingleAttempt || !nested.Stream {
		t.Fatalf("nested execution controls = %#v", nested)
	}
	if nested.EntryProtocol != protocolOpenAIImage || nested.ExitProtocol != protocolOpenAIImage {
		t.Fatalf("nested protocols = %q/%q", nested.EntryProtocol, nested.ExitProtocol)
	}
	if nested.ForcedProvider != "codex" || nested.AuthID != "codex-account-1" {
		t.Fatalf("nested provider/auth = %q/%q", nested.ForcedProvider, nested.AuthID)
	}
	if nested.Headers.Get("Authorization") != "" {
		t.Fatal("client smart key leaked to nested image execution")
	}
	if got := nested.Headers.Get("Content-Type"); got != "multipart/form-data; boundary=BravoBoundary" {
		t.Fatalf("nested content type = %q", got)
	}

	textNested := nestedHostModelRequest(req, attempt, protocolOpenAI, "gpt-5.4", []byte(`{}`), false)
	if textNested.AllowImageModel {
		t.Fatal("text execution unexpectedly allows image-only models")
	}
}

func TestRewriteOpenAIImageJSONAndResponseIdentity(t *testing.T) {
	t.Parallel()

	request, errRewrite := rewriteCandidateRequest(
		[]byte(`{"model":"bravo/image","prompt":"paint a red fox"}`),
		protocolOpenAIImage,
		"gpt-image-2",
		true,
		"application/json",
	)
	if errRewrite != nil {
		t.Fatalf("rewriteCandidateRequest() error = %v", errRewrite)
	}
	if !bytes.Contains(request, []byte(`"model":"gpt-image-2"`)) ||
		!bytes.Contains(request, []byte(`"stream":true`)) {
		t.Fatalf("rewritten request = %s", request)
	}

	response := rewriteResponseModel(
		[]byte(`{"created":1,"model":"gpt-image-2","data":[{"b64_json":"AA=="}]}`),
		"gpt-image-2",
		"bravo/image",
	)
	if !bytes.Contains(response, []byte(`"model":"bravo/image"`)) {
		t.Fatalf("rewritten response = %s", response)
	}
}

func TestRewriteOpenAIImageMultipartPreservesFilesAndBoundary(t *testing.T) {
	t.Parallel()

	var original bytes.Buffer
	writer := multipart.NewWriter(&original)
	if errBoundary := writer.SetBoundary("BravoBoundary"); errBoundary != nil {
		t.Fatal(errBoundary)
	}
	if errWrite := writer.WriteField("model", "bravo/gpt-image-1.5"); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := writer.WriteField("prompt", "replace the sky"); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatal(errWrite)
	}
	file, errFile := writer.CreateFormFile("image", "input.png")
	if errFile != nil {
		t.Fatal(errFile)
	}
	if _, errWrite := file.Write([]byte{0x89, 0x50, 0x4e, 0x47}); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	contentType := writer.FormDataContentType()

	rewritten, errRewrite := rewriteCandidateRequest(
		original.Bytes(),
		protocolOpenAIImage,
		"gpt-image-1.5",
		false,
		contentType,
	)
	if errRewrite != nil {
		t.Fatalf("rewriteCandidateRequest() error = %v", errRewrite)
	}
	reader := multipart.NewReader(bytes.NewReader(rewritten), "BravoBoundary")
	form, errRead := reader.ReadForm(1 << 20)
	if errRead != nil {
		t.Fatalf("ReadForm() error = %v", errRead)
	}
	defer form.RemoveAll()

	if got := firstFormValue(form.Value, "model"); got != "gpt-image-1.5" {
		t.Fatalf("rewritten model = %q", got)
	}
	if got := firstFormValue(form.Value, "prompt"); got != "replace the sky" {
		t.Fatalf("rewritten prompt = %q", got)
	}
	if values := form.Value["stream"]; len(values) != 0 {
		t.Fatalf("non-stream request retained stream field %v", values)
	}
	files := form.File["image"]
	if len(files) != 1 {
		t.Fatalf("image files = %d", len(files))
	}
	opened, errOpen := files[0].Open()
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	payload, errReadFile := io.ReadAll(opened)
	_ = opened.Close()
	if errReadFile != nil {
		t.Fatal(errReadFile)
	}
	if !bytes.Equal(payload, []byte{0x89, 0x50, 0x4e, 0x47}) {
		t.Fatalf("image payload = %x", payload)
	}
}

func TestOpenAIImageStreamRewritesLogicalModelIdentity(t *testing.T) {
	t.Parallel()

	rewriter := streamModelRewriter{
		physical: "gpt-image-2",
		logical:  "bravo/image",
		protocol: protocolOpenAIImage,
	}
	chunks := rewriter.Push([]byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"model\":\"gpt-image-2\",\"b64_json\":\"AA==\"}\n\n"))
	if len(chunks) != 1 {
		t.Fatalf("rewritten chunks = %d", len(chunks))
	}
	if !bytes.Contains(chunks[0], []byte(`"model":"bravo/image"`)) {
		t.Fatalf("rewritten chunk = %s", chunks[0])
	}
	if tail := rewriter.Flush(); len(tail) != 0 {
		t.Fatalf("unexpected stream tail = %q", tail)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func firstFormValue(values map[string][]string, key string) string {
	if items := values[key]; len(items) > 0 {
		return items[0]
	}
	return ""
}
