package auth

import "strings"

// displayLabelKeys lists the auth-file metadata keys consulted for a credential's
// display label, in precedence order.
//
// The note deliberately outranks every identity field. One mailbox can back
// several distinct credentials — two Claude accounts on the same address that
// differ only by workspace render identically in every list labelled by email —
// and the note is the only place an operator can record that difference. An
// explicit "label" comes next for stores that persist one, then the identity
// fields as a last resort.
var displayLabelKeys = []string{"note", "label", "email", "project_id"}

// DisplayLabelFromMetadata derives the human-readable credential label from raw
// auth-file metadata. It returns an empty string when metadata carries none of
// the known keys, leaving the choice of fallback to the caller.
func DisplayLabelFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range displayLabelKeys {
		value, ok := metadata[key].(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// DisplayLabelMetadataKeys returns the label keys in precedence order for callers
// that store metadata values as something other than plain strings and therefore
// need their own coercion.
func DisplayLabelMetadataKeys() []string {
	return append([]string(nil), displayLabelKeys...)
}
