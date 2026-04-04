package cmd

import (
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

func TestStripAndFilterPrefix(t *testing.T) {
	tests := []struct {
		name       string
		ch         types.ChangeEntry
		prefix     string
		wantAction string
		wantBefore string
		wantAfter  string
	}{
		{
			name:       "put in scope",
			ch:         types.ChangeEntry{Action: "put", PathAfter: "/docs/readme.md"},
			prefix:     "docs/",
			wantAction: "put",
			wantAfter:  "readme.md",
		},
		{
			name:       "put out of scope",
			ch:         types.ChangeEntry{Action: "put", PathAfter: "/other/file.txt"},
			prefix:     "docs/",
			wantAction: "",
		},
		{
			name:       "delete in scope",
			ch:         types.ChangeEntry{Action: "delete", PathBefore: "/docs/old.txt"},
			prefix:     "docs/",
			wantAction: "delete",
			wantBefore: "old.txt",
		},
		{
			name:       "delete out of scope",
			ch:         types.ChangeEntry{Action: "delete", PathBefore: "/other/old.txt"},
			prefix:     "docs/",
			wantAction: "",
		},
		{
			name:       "move both in scope",
			ch:         types.ChangeEntry{Action: "move", PathBefore: "/docs/a.txt", PathAfter: "/docs/b.txt"},
			prefix:     "docs/",
			wantAction: "move",
			wantBefore: "a.txt",
			wantAfter:  "b.txt",
		},
		{
			name:       "move out of scope → delete",
			ch:         types.ChangeEntry{Action: "move", PathBefore: "/docs/a.txt", PathAfter: "/archive/a.txt"},
			prefix:     "docs/",
			wantAction: "delete",
			wantBefore: "a.txt",
			wantAfter:  "",
		},
		{
			name:       "move into scope → put",
			ch:         types.ChangeEntry{Action: "move", PathBefore: "/archive/a.txt", PathAfter: "/docs/a.txt"},
			prefix:     "docs/",
			wantAction: "put",
			wantBefore: "",
			wantAfter:  "a.txt",
		},
		{
			name:       "move both out of scope → skip",
			ch:         types.ChangeEntry{Action: "move", PathBefore: "/other/a.txt", PathAfter: "/archive/a.txt"},
			prefix:     "docs/",
			wantAction: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripAndFilterPrefix(tt.ch, tt.prefix)
			if got.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", got.Action, tt.wantAction)
			}
			if tt.wantAction != "" {
				if got.PathBefore != tt.wantBefore {
					t.Errorf("PathBefore = %q, want %q", got.PathBefore, tt.wantBefore)
				}
				if got.PathAfter != tt.wantAfter {
					t.Errorf("PathAfter = %q, want %q", got.PathAfter, tt.wantAfter)
				}
			}
		})
	}
}
