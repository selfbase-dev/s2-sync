package sync

import (
	"path/filepath"
	"testing"
)

func TestConflictFileName(t *testing.T) {
	tests := []struct {
		input    string
		contains string // partial match since timestamp varies
	}{
		{"report.txt", ".sync-conflict-"},
		{"report.txt", ".txt"},         // extension preserved
		{"Makefile", ".sync-conflict-"}, // no extension
	}
	for _, tt := range tests {
		got := conflictFileName(tt.input)
		if len(got) == 0 {
			t.Errorf("conflictFileName(%q) returned empty", tt.input)
			continue
		}
		found := false
		if filepath.Ext(tt.input) != "" {
			// Should end with original extension
			if filepath.Ext(got) != filepath.Ext(tt.input) {
				t.Errorf("conflictFileName(%q) = %q, extension not preserved", tt.input, got)
			}
		}
		for _, s := range []string{tt.contains} {
			if len(s) > 0 {
				found = true
				if !containsStr(got, s) {
					t.Errorf("conflictFileName(%q) = %q, should contain %q", tt.input, got, s)
				}
			}
		}
		_ = found
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

