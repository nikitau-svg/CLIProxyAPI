package claude

import "testing"

func TestCredentialFileNameSeparatesOrganizationsWithSameEmail(t *testing.T) {
	personal := CredentialFileName("same@example.com", "org-personal")
	team := CredentialFileName("same@example.com", "org-team")

	if personal == team {
		t.Fatalf("expected distinct filenames, got %q", personal)
	}
	if personal != "claude-same@example.com--org-personal.json" {
		t.Fatalf("unexpected personal filename: %q", personal)
	}
	if team != "claude-same@example.com--org-team.json" {
		t.Fatalf("unexpected team filename: %q", team)
	}
}

func TestCredentialFileNameKeepsLegacyFallback(t *testing.T) {
	if got := CredentialFileName("same@example.com", ""); got != "claude-same@example.com.json" {
		t.Fatalf("unexpected legacy filename: %q", got)
	}
}

func TestCredentialFileNameSanitizesSegments(t *testing.T) {
	got := CredentialFileName(" user/name@example.com ", " org/id ")
	if got != "claude-user-name@example.com--org-id.json" {
		t.Fatalf("unexpected sanitized filename: %q", got)
	}
}
