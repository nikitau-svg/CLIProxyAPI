package auth

import "testing"

func TestDisplayLabelFromMetadataPrefersNote(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{
			name: "note outranks email",
			metadata: map[string]any{
				"email": "nikita.u@slowdive.app",
				"note":  "Личный аккаунт Claude",
			},
			want: "Личный аккаунт Claude",
		},
		{
			// The case the precedence exists for: two credentials on one mailbox that
			// differ only by workspace are one indistinguishable name without the note.
			name: "second credential on the same mailbox keeps its own name",
			metadata: map[string]any{
				"email":             "nikita.u@slowdive.app",
				"organization_name": "Ascetix, inc",
				"note":              "Рабочий аккаунт Claude",
			},
			want: "Рабочий аккаунт Claude",
		},
		{
			name:     "email when no note",
			metadata: map[string]any{"email": "nikita.u@slowdive.app"},
			want:     "nikita.u@slowdive.app",
		},
		{
			name:     "blank note falls through",
			metadata: map[string]any{"note": "   ", "email": "a@b.c"},
			want:     "a@b.c",
		},
		{
			name:     "explicit label beats email",
			metadata: map[string]any{"label": "stored", "email": "a@b.c"},
			want:     "stored",
		},
		{
			name:     "project id as last resort",
			metadata: map[string]any{"project_id": "proj-1"},
			want:     "proj-1",
		},
		{
			name:     "nothing label-worthy",
			metadata: map[string]any{"type": "claude"},
			want:     "",
		},
		{
			name:     "nil metadata",
			metadata: nil,
			want:     "",
		},
		{
			name:     "non-string note is skipped",
			metadata: map[string]any{"note": 42, "email": "a@b.c"},
			want:     "a@b.c",
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			if got := DisplayLabelFromMetadata(testCase.metadata); got != testCase.want {
				t.Fatalf("DisplayLabelFromMetadata() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// The Postgres store resolves the same precedence itself because its metadata
// values need coercion first, so it reads the key order from here.
func TestDisplayLabelMetadataKeysOrderAndIsolation(t *testing.T) {
	keys := DisplayLabelMetadataKeys()
	want := []string{"note", "label", "email", "project_id"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	keys[0] = "mutated"
	if DisplayLabelMetadataKeys()[0] != "note" {
		t.Fatal("caller mutated the shared precedence list")
	}
}
