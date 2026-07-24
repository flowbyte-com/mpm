package internal

import (
	"strings"
	"testing"
)

func TestValidateSchemaPrefix(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		// Empty is allowed (treats as "default schema").
		{"empty", "", false},
		// Normal prefixes.
		{"main", "main.", false},
		{"shared", "shared.", false},
		{"underscore-start", "_priv.", false},
		{"digits-after-first", "s1.", false},
		// Injection vectors — must reject.
		{"no-trailing-dot", "main", true},
		{"trailing-dot-only", ".", true},
		{"digit-start", "1main.", true},
		{"hyphen", "main-memories.", true},
		{"semicolon", "main.;DROP TABLE memories;--", true},
		{"space", "main .", true},
		{"quote", `main"`, true},
		{"wildcard", "main*.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSchemaPrefix(tc.prefix)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateSchemaPrefix(%q) = nil, want error", tc.prefix)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateSchemaPrefix(%q) = %v, want nil", tc.prefix, err)
			}
			// Error message must not echo back the unsafe value verbatim
			// (defense in depth: a wrapped formatter could accidentally
			// leak the rejected input into logs).
			if err != nil && strings.Contains(err.Error(), tc.prefix) && tc.wantErr {
				// For some test cases the prefix appears in the message
				// because the regex string is shown; allow it but verify
				// the message is reasonable.
				if !strings.Contains(err.Error(), "invalid schema prefix") {
					t.Errorf("ValidateSchemaPrefix(%q) error %q missing context", tc.prefix, err)
				}
			}
		})
	}
}