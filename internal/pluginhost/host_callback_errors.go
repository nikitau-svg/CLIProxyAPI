package pluginhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type hostModelCallbackError struct {
	cause      error
	code       string
	statusCode int
	retryable  bool
	headers    http.Header
	retryAfter string
}

func (e *hostModelCallbackError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	if e.statusCode > 0 {
		return fmt.Sprintf("model execution failed with status %d", e.statusCode)
	}
	return "model execution failed"
}

func (e *hostModelCallbackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *hostModelCallbackError) ErrorCode() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *hostModelCallbackError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

func (e *hostModelCallbackError) Retryable() bool {
	return e != nil && e.retryable
}

func (e *hostModelCallbackError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHeader(e.headers)
}

func (e *hostModelCallbackError) RetryAfterValue() string {
	if e == nil {
		return ""
	}
	return e.retryAfter
}

func newHostModelCallbackError(errMsg *interfaces.ErrorMessage) error {
	if errMsg == nil {
		return nil
	}
	cause := errMsg.Error
	statusCode := errMsg.StatusCode
	headers := cloneHeader(errMsg.Addon)
	code := ""
	retryable := false
	retryableKnown := false

	var authErr *coreauth.Error
	if errors.As(cause, &authErr) && authErr != nil {
		code = strings.TrimSpace(authErr.Code)
		retryable = authErr.Retryable
		retryableKnown = true
		if statusCode == 0 {
			statusCode = authErr.HTTPStatus
		}
	}
	var coded interface{ ErrorCode() string }
	if code == "" && errors.As(cause, &coded) && coded != nil {
		code = strings.TrimSpace(coded.ErrorCode())
	}
	var statusProvider interface{ StatusCode() int }
	if statusCode == 0 && errors.As(cause, &statusProvider) && statusProvider != nil {
		statusCode = statusProvider.StatusCode()
	}
	var retryableProvider interface{ Retryable() bool }
	if !retryableKnown && errors.As(cause, &retryableProvider) && retryableProvider != nil {
		retryable = retryableProvider.Retryable()
		retryableKnown = true
	}
	var headerProvider interface{ Headers() http.Header }
	if errors.As(cause, &headerProvider) && headerProvider != nil {
		headers = mergeHostErrorHeaders(headers, headerProvider.Headers())
	}
	if !retryableKnown {
		retryable = isRetryableHostStatus(statusCode)
	}

	retryAfter := strings.TrimSpace(headers.Get("Retry-After"))
	if retryAfter == "" {
		var retryAfterValueProvider interface{ RetryAfterValue() string }
		if errors.As(cause, &retryAfterValueProvider) && retryAfterValueProvider != nil {
			retryAfter = strings.TrimSpace(retryAfterValueProvider.RetryAfterValue())
		}
	}
	if retryAfter == "" {
		var retryAfterDurationProvider interface{ RetryAfter() *time.Duration }
		if errors.As(cause, &retryAfterDurationProvider) && retryAfterDurationProvider != nil {
			if duration := retryAfterDurationProvider.RetryAfter(); duration != nil {
				seconds := int64(math.Ceil(duration.Seconds()))
				if seconds < 0 {
					seconds = 0
				}
				retryAfter = strconv.FormatInt(seconds, 10)
			}
		}
	}
	if code == "" {
		code = "model_execution_failed"
	}
	return &hostModelCallbackError{
		cause:      cause,
		code:       code,
		statusCode: statusCode,
		retryable:  retryable,
		headers:    headers,
		retryAfter: retryAfter,
	}
}

func mergeHostErrorHeaders(base, extra http.Header) http.Header {
	if len(extra) == 0 {
		return base
	}
	if base == nil {
		base = make(http.Header, len(extra))
	}
	for key, values := range extra {
		base.Del(key)
		for _, value := range values {
			base.Add(key, value)
		}
	}
	return base
}

func isRetryableHostStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func marshalHostCallbackError(err error) []byte {
	if err == nil {
		return marshalRPCError("host_call_failed", "host callback failed")
	}
	detail := &pluginabi.Error{
		Code:    "host_call_failed",
		Message: err.Error(),
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) && coded != nil {
		if code := strings.TrimSpace(coded.ErrorCode()); code != "" {
			detail.Code = code
		}
	}
	var statusProvider interface{ StatusCode() int }
	if errors.As(err, &statusProvider) && statusProvider != nil {
		detail.HTTPStatus = statusProvider.StatusCode()
	}
	var retryableProvider interface{ Retryable() bool }
	if errors.As(err, &retryableProvider) && retryableProvider != nil {
		detail.Retryable = retryableProvider.Retryable()
	}
	var headerProvider interface{ Headers() http.Header }
	if errors.As(err, &headerProvider) && headerProvider != nil {
		detail.Headers = cloneHeader(headerProvider.Headers())
	}
	var retryAfterProvider interface{ RetryAfterValue() string }
	if errors.As(err, &retryAfterProvider) && retryAfterProvider != nil {
		detail.RetryAfter = strings.TrimSpace(retryAfterProvider.RetryAfterValue())
	}
	if detail.RetryAfter == "" {
		detail.RetryAfter = strings.TrimSpace(detail.Headers.Get("Retry-After"))
	}
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: detail})
	return raw
}
