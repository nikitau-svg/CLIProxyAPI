package claude

import (
	"fmt"
	"strings"
)

// CredentialFileName returns the filename used to persist Claude OAuth credentials.
// The organization UUID keeps personal and team workspaces with the same email distinct.
// The legacy email-only filename remains the fallback for older token responses.
func CredentialFileName(email, organizationUUID string) string {
	email = sanitizeCredentialFileSegment(email)
	organizationUUID = sanitizeCredentialFileSegment(organizationUUID)
	if organizationUUID == "" {
		return fmt.Sprintf("claude-%s.json", email)
	}
	return fmt.Sprintf("claude-%s--%s.json", email, organizationUUID)
}

func sanitizeCredentialFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@' || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
