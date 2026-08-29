package config

import (
	"fmt"
	"strings"
)

func normalizeErrorLogCapture(cfg *ErrorLogCaptureConfig) error {
	if cfg == nil {
		return nil
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch cfg.Mode {
	case "", ErrorLogCaptureModeMetadata:
		cfg.Mode = ErrorLogCaptureModeMetadata
		return nil
	case ErrorLogCaptureModeOff:
		return nil
	case ErrorLogCaptureModeBody:
		return fmt.Errorf("error-log-capture.mode body is not available yet; use metadata or off")
	default:
		return fmt.Errorf("error-log-capture.mode must be metadata or off")
	}
}
