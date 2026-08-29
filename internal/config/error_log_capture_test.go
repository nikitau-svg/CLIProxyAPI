package config

import (
	"strings"
	"testing"
)

func TestParseConfigBytesErrorLogCapture(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantMode string
		wantErr  string
	}{
		{
			name:     "missing section defaults to metadata",
			yaml:     "port: 8317\n",
			wantMode: ErrorLogCaptureModeMetadata,
		},
		{
			name:     "metadata accepted",
			yaml:     "error-log-capture:\n  mode: metadata\n",
			wantMode: ErrorLogCaptureModeMetadata,
		},
		{
			name:     "off accepted case insensitively",
			yaml:     "error-log-capture:\n  mode: OFF\n",
			wantMode: ErrorLogCaptureModeOff,
		},
		{
			name:    "body rejected until bounded implementation exists",
			yaml:    "error-log-capture:\n  mode: body\n",
			wantErr: "body is not available yet",
		},
		{
			name:    "unknown mode rejected",
			yaml:    "error-log-capture:\n  mode: everything\n",
			wantErr: "must be metadata or off",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(test.yaml))
			if test.wantErr != "" {
				if errParse == nil || !strings.Contains(errParse.Error(), test.wantErr) {
					t.Fatalf("ParseConfigBytes error = %v, want substring %q", errParse, test.wantErr)
				}
				return
			}
			if errParse != nil {
				t.Fatalf("ParseConfigBytes: %v", errParse)
			}
			if cfg.ErrorLogCapture.Mode != test.wantMode {
				t.Fatalf("mode = %q, want %q", cfg.ErrorLogCapture.Mode, test.wantMode)
			}
		})
	}
}
