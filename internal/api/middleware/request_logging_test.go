package middleware

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func TestShouldSkipMethodForRequestLogging(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		skip bool
	}{
		{
			name: "nil request",
			req:  nil,
			skip: true,
		},
		{
			name: "post request should not skip",
			req: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Path: "/v1/responses"},
			},
			skip: false,
		},
		{
			name: "plain get should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/models"},
				Header: http.Header{},
			},
			skip: true,
		},
		{
			name: "responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "codex responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/backend-api/codex/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "responses get without upgrade should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{},
			},
			skip: true,
		},
	}

	for i := range tests {
		got := shouldSkipMethodForRequestLogging(tests[i].req)
		if got != tests[i].skip {
			t.Fatalf("%s: got skip=%t, want %t", tests[i].name, got, tests[i].skip)
		}
	}
}

func TestShouldCaptureRequestBody(t *testing.T) {
	tests := []struct {
		name          string
		loggerEnabled bool
		req           *http.Request
		want          bool
	}{
		{
			name:          "logger enabled always captures",
			loggerEnabled: true,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "nil request",
			loggerEnabled: false,
			req:           nil,
			want:          false,
		},
		{
			name:          "small known size json in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: 2,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "large known size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: 2<<20 + 1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "unknown size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "multipart skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: 1,
				Header:        http.Header{"Content-Type": []string{"multipart/form-data; boundary=abc"}},
			},
			want: false,
		},
	}

	for i := range tests {
		got := shouldCaptureRequestBody(tests[i].loggerEnabled, tests[i].req)
		if got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestRequestBodyMetadataCaptureIsTransparentAndCountsConsumption(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("remaining-body"))
	request.ContentLength = int64(len("remaining-body"))
	requestInfo := &RequestInfo{}
	capture := attachRequestBodyMetadata(request, requestInfo)
	if capture == nil {
		t.Fatal("request body metadata capture was not attached")
	}

	first := make([]byte, 3)
	n, errRead := request.Body.Read(first)
	if errRead != nil {
		t.Fatalf("read first request bytes: %v", errRead)
	}
	if got := string(first[:n]); got != "rem" {
		t.Fatalf("first read = %q, want rem", got)
	}
	partial := capture.snapshot()
	if partial.ConsumedBytes != 3 || partial.DeclaredBytes != int64(len("remaining-body")) || partial.Complete {
		t.Fatalf("partial metadata = %+v", partial)
	}

	rest, errRemaining := io.ReadAll(request.Body)
	if errRemaining != nil {
		t.Fatalf("read remaining body: %v", errRemaining)
	}
	if got := string(first[:n]) + string(rest); got != "remaining-body" {
		t.Fatalf("handler body = %q, want remaining-body", got)
	}
	complete := capture.snapshot()
	if complete.ConsumedBytes != int64(len("remaining-body")) || !complete.Complete {
		t.Fatalf("complete metadata = %+v", complete)
	}
}

func TestRequestLoggingMiddlewareLargeErrorKeepsOnlyMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	payload := append([]byte(`{"marker":"large-error-body","padding":"`), bytes.Repeat([]byte("x"), 2<<20)...)
	payload = append(payload, []byte(`"}`)...)
	upstreamBody := []byte(`{"model":"upstream-model","input":"translated"}`)

	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		if !bytes.Equal(body, payload) {
			c.Status(http.StatusInternalServerError)
			return
		}
		executorCtx := context.WithValue(context.Background(), "gin", c)
		helps.RecordAPIRequest(executorCtx, &config.Config{}, helps.UpstreamRequestLog{
			URL:     "https://api.example.com/v1/responses",
			Method:  http.MethodPost,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    upstreamBody,
		})
		c.JSON(http.StatusBadRequest, gin.H{"error": "upstream rejected request"})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	var logPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			logPath = logsDir + string(os.PathSeparator) + entry.Name()
			break
		}
	}
	if logPath == "" {
		t.Fatal("forced error log was not created")
	}
	content, errReadLog := os.ReadFile(logPath)
	if errReadLog != nil {
		t.Fatalf("read error log: %v", errReadLog)
	}
	if bytes.Contains(content, payload) {
		t.Fatal("production error log leaked the complete large request body")
	}
	if !bytes.Contains(content, []byte("[OMITTED: production error logs do not persist request bodies]")) {
		t.Fatal("production error log does not explain that the request body was omitted")
	}
	if bytes.Contains(content, []byte("=== API REQUEST 1 ===")) || bytes.Contains(content, upstreamBody) {
		t.Fatal("production error log leaked the deferred upstream request")
	}
	metadata := []byte(`"declared_bytes":` + fmt.Sprint(len(payload)) + `,"consumed_bytes":` + fmt.Sprint(len(payload)) + `,"complete":true`)
	if !bytes.Contains(content, metadata) {
		t.Fatalf("production error log metadata = %q, want %q", content, metadata)
	}
	responseMetadata := []byte(`"written_bytes":` + fmt.Sprint(response.Body.Len()))
	if !bytes.Contains(content, responseMetadata) {
		t.Fatalf("production response metadata = %q, want %q", content, responseMetadata)
	}
	info, errStat := os.Stat(logPath)
	if errStat != nil {
		t.Fatalf("stat error log: %v", errStat)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("error log mode = %04o, want 0600", got)
	}
	if len(entries) != 1 {
		t.Fatalf("logs directory contains temp/spool files: %v", entries)
	}
}

func TestRequestLoggingMiddlewareMetadataSuccessCreatesNoFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	payload := bytes.Repeat([]byte("unknown-length-body"), 100000)

	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil || !bytes.Equal(body, payload) {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	request.ContentLength = -1
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusNoContent)
	}
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	if len(entries) != 0 {
		t.Fatalf("successful metadata request created files: %v", entries)
	}
}

func TestRequestLoggingMiddlewareMetadataCountsPartialBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	payload := []byte("secret-request-body")

	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		consumed := make([]byte, 3)
		if _, errRead := io.ReadFull(c.Request.Body, consumed); errRead != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusBadRequest)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errReadDir)
	}
	content, errReadLog := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errReadLog != nil {
		t.Fatalf("read error log: %v", errReadLog)
	}
	if bytes.Contains(content, payload) {
		t.Fatal("error log retained a partially consumed request body")
	}
	for _, want := range []string{`"declared_bytes":19`, `"consumed_bytes":3`, `"complete":false`} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("error log missing %q: %s", want, content)
		}
	}
}

func TestRequestLoggingMiddlewareOffCreatesNoErrorFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	logger.SetErrorLogCaptureMode(config.ErrorLogCaptureModeOff)
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusBadRequest) })

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("secret")))
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	if len(entries) != 0 {
		t.Fatalf("off mode created files: %v", entries)
	}
}

func TestRequestLoggingMiddlewareCustomLoggerFailsClosedForMetadataCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger := &testRequestLogger{enabled: false}
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusBadRequest) })

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("secret")))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if logger.requests != 0 {
		t.Fatalf("legacy custom logger received %d forced metadata logs, want 0", logger.requests)
	}
}

func TestRequestLoggingMiddlewareSnapshotsMetadataPolicyPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	payload := []byte("request-secret")
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		if _, errRead := io.ReadAll(c.Request.Body); errRead != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		// Simulate a hot reload while this request is in flight. The request
		// must finish under its original metadata policy.
		logger.SetErrorLogCaptureMode(config.ErrorLogCaptureModeOff)
		logger.SetEnabled(true)
		c.Status(http.StatusBadRequest)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload)))
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil || len(entries) != 1 {
		t.Fatalf("error log entries = %v, err=%v", entries, errReadDir)
	}
	if !strings.HasPrefix(entries[0].Name(), "error-") {
		t.Fatalf("hot reload changed forced log filename: %s", entries[0].Name())
	}
	content, errReadLog := os.ReadFile(filepath.Join(logsDir, entries[0].Name()))
	if errReadLog != nil {
		t.Fatalf("read error log: %v", errReadLog)
	}
	if bytes.Contains(content, payload) || !bytes.Contains(content, []byte(`"consumed_bytes":14`)) {
		t.Fatalf("hot reload changed metadata policy: %s", content)
	}
}

func TestRequestLoggingMiddlewareMetadataHotReloadDoesNotStartStreamingLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 10)
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		// Simulate request-log being enabled after this request already entered
		// metadata mode but before the first streaming response header.
		logger.SetEnabled(true)
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write([]byte("data: safe\n\n"))
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("secret")))
	if response.Code != http.StatusOK || response.Body.String() != "data: safe\n\n" {
		t.Fatalf("stream response = %d %q", response.Code, response.Body.String())
	}
	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("read logs dir: %v", errReadDir)
	}
	if len(entries) != 0 {
		t.Fatalf("metadata request started a streaming logger after hot reload: %v", entries)
	}
}

func TestAttachRequestLogSourcesUsesLoggerLogsDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 0)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/backend-api/codex/responses", nil)
	c.Request.Header.Set("Upgrade", "websocket")

	attachRequestLogSources(c, logger, true)
	defer cleanupFileBodySourcesFromContext(c)

	for _, key := range []string{
		logging.WebsocketTimelineSourceContextKey,
		logging.APIWebsocketTimelineSourceContextKey,
	} {
		value, exists := c.Get(key)
		if !exists {
			t.Fatalf("expected %s source to be attached", key)
		}
		source, ok := value.(*logging.FileBodySource)
		if !ok || source == nil {
			t.Fatalf("%s source type = %T", key, value)
		}
		file, errPart := source.CreatePart("probe")
		if errPart != nil {
			t.Fatalf("CreatePart(%s): %v", key, errPart)
		}
		path := file.Name()
		if errClose := file.Close(); errClose != nil {
			t.Fatalf("close part: %v", errClose)
		}
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("%s part path %s is not under logs dir %s", key, path, logsDir)
		}
	}
}

func cleanupFileBodySourcesFromContext(c *gin.Context) {
	if c == nil {
		return
	}
	for _, key := range []string{
		logging.WebsocketTimelineSourceContextKey,
		logging.APIWebsocketTimelineSourceContextKey,
	} {
		value, exists := c.Get(key)
		if !exists {
			continue
		}
		if source, ok := value.(*logging.FileBodySource); ok && source != nil {
			_ = source.Cleanup()
		}
	}
}

func TestDecodeCapturedRequestBodyForLogWithLimitTruncatesZstdExpansion(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}

	decoded := decodeCapturedRequestBodyForLogWithLimit(compressed.Bytes(), "zstd", 64)
	if len(decoded) > 128 {
		t.Fatalf("limited decoded body length = %d, want bounded output", len(decoded))
	}
	if !bytes.Contains(decoded, []byte("DECOMPRESSED REQUEST BODY TRUNCATED")) {
		t.Fatalf("decoded body = %q, want truncation marker", string(decoded))
	}
}

func TestCaptureRequestInfoDecodesZstdRequestBodyForLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model","stream":true}`)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	compressedBytes := compressed.Bytes()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedBytes))
	req.Header.Set("Content-Encoding", "zstd")
	c.Request = req

	info, errCapture := captureRequestInfo(c, true)
	if errCapture != nil {
		t.Fatalf("captureRequestInfo: %v", errCapture)
	}
	if !bytes.Equal(info.Body, payload) {
		t.Fatalf("logged request body = %q, want %q", string(info.Body), string(payload))
	}

	restoredBody, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		t.Fatalf("read restored request body: %v", errRead)
	}
	if !bytes.Equal(restoredBody, compressedBytes) {
		t.Fatal("request body was not restored with the original compressed bytes")
	}
}
