// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// RequestLoggingMiddleware creates a Gin middleware that logs HTTP requests and responses.
// It captures detailed information about the request and response, including headers and body,
// and uses the provided RequestLogger to record this data. When full request logging is disabled,
// metadata mode counts body consumption without retaining or spooling body bytes.
func RequestLoggingMiddleware(logger logging.RequestLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logger == nil {
			c.Next()
			return
		}

		if shouldSkipMethodForRequestLogging(c.Request) {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			c.Next()
			return
		}

		loggerEnabled := logger.IsEnabled()
		errorCaptureMode := snapshotErrorLogCaptureMode(logger)
		if !loggerEnabled && errorCaptureMode == config.ErrorLogCaptureModeOff {
			c.Next()
			return
		}
		captureBody := shouldCaptureRequestBody(loggerEnabled, c.Request)

		// Capture request information
		requestInfo, err := captureRequestInfo(c, captureBody)
		if err != nil {
			// Log error but continue processing
			// In a real implementation, you might want to use a proper logger here
			c.Next()
			return
		}

		// Create response writer wrapper
		wrapper := NewResponseWriterWrapper(c.Writer, logger, requestInfo)
		if !loggerEnabled {
			wrapper.logOnErrorOnly = true
		}
		c.Writer = wrapper
		attachRequestLogSources(c, logger, loggerEnabled)
		if !loggerEnabled && errorCaptureMode == config.ErrorLogCaptureModeMetadata {
			attachRequestBodyMetadata(c.Request, requestInfo)
		}

		// Process the request
		c.Next()

		// Finalize logging after request processing
		if err = wrapper.Finalize(c); err != nil {
			// Log error but don't interrupt the response
			// In a real implementation, you might want to use a proper logger here
		}
	}
}

func snapshotErrorLogCaptureMode(logger logging.RequestLogger) string {
	// Error-only metadata must be an explicit closed contract. A custom logger
	// that only implements the legacy RequestLogger interface must not receive a
	// force flag through an incompatible fallback and accidentally persist raw
	// upstream data.
	if provider, ok := logger.(interface {
		ErrorLogCaptureMode() string
		SupportsSafeErrorLogCapture() bool
	}); ok && provider.SupportsSafeErrorLogCapture() {
		switch provider.ErrorLogCaptureMode() {
		case config.ErrorLogCaptureModeOff:
			return config.ErrorLogCaptureModeOff
		case config.ErrorLogCaptureModeMetadata:
			return config.ErrorLogCaptureModeMetadata
		}
	}
	return config.ErrorLogCaptureModeOff
}

type fileBodySourceFactory interface {
	NewFileBodySource(prefix string) (*logging.FileBodySource, error)
}

type requestBodyMetadataCapture struct {
	body          io.ReadCloser
	contentLength int64
	bytesRead     int64
	sawEOF        bool
}

func attachRequestBodyMetadata(req *http.Request, requestInfo *RequestInfo) *requestBodyMetadataCapture {
	if req == nil || req.Body == nil || req.Body == http.NoBody || requestInfo == nil {
		return nil
	}
	capture := &requestBodyMetadataCapture{body: req.Body, contentLength: req.ContentLength}
	req.Body = capture
	requestInfo.bodyMetadata = capture
	return capture
}

func (c *requestBodyMetadataCapture) Read(payload []byte) (int, error) {
	if c == nil || c.body == nil {
		return 0, io.EOF
	}
	n, errRead := c.body.Read(payload)
	c.bytesRead += int64(n)
	if errRead == io.EOF {
		c.sawEOF = true
	}
	return n, errRead
}

func (c *requestBodyMetadataCapture) Close() error {
	if c == nil {
		return nil
	}
	if c.body == nil {
		return nil
	}
	return c.body.Close()
}

func (c *requestBodyMetadataCapture) snapshot() logging.ErrorRequestBodyMetadata {
	if c == nil {
		return logging.ErrorRequestBodyMetadata{}
	}
	complete := c.sawEOF || (c.contentLength >= 0 && c.bytesRead >= c.contentLength)
	declared := c.contentLength
	if declared < -1 {
		declared = -1
	}
	return logging.ErrorRequestBodyMetadata{
		DeclaredBytes: declared,
		ConsumedBytes: max(c.bytesRead, 0),
		Complete:      complete,
	}
}

func attachRequestLogSources(c *gin.Context, logger logging.RequestLogger, loggerEnabled bool) {
	if c == nil || !loggerEnabled {
		return
	}
	factory, ok := logger.(fileBodySourceFactory)
	if !ok || factory == nil {
		return
	}
	if source, errSource := factory.NewFileBodySource("api-request"); errSource == nil {
		c.Set(logging.APIRequestSourceContextKey, source)
	}
	if source, errSource := factory.NewFileBodySource("api-response"); errSource == nil {
		c.Set(logging.APIResponseSourceContextKey, source)
	}
	if !isResponsesWebsocketUpgrade(c.Request) {
		return
	}
	if source, errSource := factory.NewFileBodySource("websocket-timeline"); errSource == nil {
		c.Set(logging.WebsocketTimelineSourceContextKey, source)
	}
	if source, errSource := factory.NewFileBodySource("api-websocket-timeline"); errSource == nil {
		c.Set(logging.APIWebsocketTimelineSourceContextKey, source)
	}
}

func shouldSkipMethodForRequestLogging(req *http.Request) bool {
	if req == nil {
		return true
	}
	if req.Method != http.MethodGet {
		return false
	}
	return !isResponsesWebsocketUpgrade(req)
}

func isResponsesWebsocketUpgrade(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.URL.Path != "/v1/responses" && req.URL.Path != "/backend-api/codex/responses" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

func shouldCaptureRequestBody(loggerEnabled bool, req *http.Request) bool {
	return loggerEnabled
}

// captureRequestInfo extracts relevant information from the incoming HTTP request.
// It captures the URL, method, headers, and body. The request body is read and then
// restored so that it can be processed by subsequent handlers.
func captureRequestInfo(c *gin.Context, captureBody bool) (*RequestInfo, error) {
	// Capture URL with sensitive query parameters masked
	maskedQuery := util.MaskSensitiveQuery(c.Request.URL.RawQuery)
	url := c.Request.URL.Path
	if maskedQuery != "" {
		url += "?" + maskedQuery
	}

	// Capture method
	method := c.Request.Method

	// Capture headers
	headers := make(map[string][]string)
	for key, values := range c.Request.Header {
		headers[key] = values
	}

	// Capture request body
	var body []byte
	if captureBody && c.Request.Body != nil {
		// Read the body
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, err
		}

		// Restore the body for the actual request processing
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		body = decodeCapturedRequestBodyForLog(bodyBytes, c.Request.Header.Get("Content-Encoding"))
	}

	return &RequestInfo{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		RequestID: logging.GetGinRequestID(c),
		Timestamp: time.Now(),
	}, nil
}

func decodeCapturedRequestBodyForLog(raw []byte, encoding string) []byte {
	if len(raw) == 0 {
		return raw
	}

	decoded, errDecode := decodeCapturedRequestBody(raw, encoding)
	if errDecode != nil {
		return raw
	}
	return decoded
}

func decodeCapturedRequestBodyForLogWithLimit(raw []byte, encoding string, limit int64) []byte {
	if len(raw) == 0 || limit <= 0 {
		return raw
	}
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw
	}

	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, truncated, errDecode := decodeCapturedZstdRequestBodyWithLimit(body, limit)
			if errDecode != nil {
				return raw
			}
			body = decoded
			if truncated {
				if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
					body = append(body, '\n')
				}
				return append(body, "[DECOMPRESSED REQUEST BODY TRUNCATED]"...)
			}
		default:
			return raw
		}
	}
	return body
}

func decodeCapturedRequestBody(raw []byte, encoding string) ([]byte, error) {
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, errDecode := decodeCapturedZstdRequestBody(body)
			if errDecode != nil {
				return nil, errDecode
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeCapturedZstdRequestBody(raw []byte) ([]byte, error) {
	decoder, errNewReader := zstd.NewReader(bytes.NewReader(raw))
	if errNewReader != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", errNewReader)
	}
	defer decoder.Close()

	decoded, errRead := io.ReadAll(decoder)
	if errRead != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", errRead)
	}
	return decoded, nil
}

func decodeCapturedZstdRequestBodyWithLimit(raw []byte, limit int64) ([]byte, bool, error) {
	decoder, errNewReader := zstd.NewReader(bytes.NewReader(raw))
	if errNewReader != nil {
		return nil, false, fmt.Errorf("failed to create zstd request decoder: %w", errNewReader)
	}
	defer decoder.Close()

	decoded, errRead := io.ReadAll(io.LimitReader(decoder, limit+1))
	if errRead != nil {
		return nil, false, fmt.Errorf("failed to decode zstd request body: %w", errRead)
	}
	if int64(len(decoded)) > limit {
		return decoded[:limit], true, nil
	}
	return decoded, false, nil
}

// shouldLogRequest determines whether the request should be logged.
// It skips management endpoints to avoid leaking secrets but allows
// all other routes, including module-provided ones, to honor request-log.
func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	return true
}
