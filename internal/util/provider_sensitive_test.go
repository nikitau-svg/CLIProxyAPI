package util

import "testing"

func TestMaskSensitiveHeaderValueDoesNotRetainCredentialFragments(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "bearer", key: "Authorization", value: "Bearer secret-prefix-and-suffix", want: "Bearer [REDACTED]"},
		{name: "basic", key: "Proxy-Authorization", value: "Basic c2VjcmV0", want: "Basic [REDACTED]"},
		{name: "bare authorization", key: "Authorization", value: "secret", want: "[REDACTED]"},
		{name: "api key", key: "X-Api-Key", value: "sk-secret", want: "[REDACTED]"},
		{name: "token", key: "X-Access-Token", value: "token-secret", want: "[REDACTED]"},
		{name: "cookie", key: "Cookie", value: "session=secret", want: "[REDACTED]"},
		{name: "ordinary", key: "Content-Type", value: "application/json", want: "application/json"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := MaskSensitiveHeaderValue(test.key, test.value); got != test.want {
				t.Fatalf("MaskSensitiveHeaderValue(%q, %q) = %q, want %q", test.key, test.value, got, test.want)
			}
		})
	}
}
