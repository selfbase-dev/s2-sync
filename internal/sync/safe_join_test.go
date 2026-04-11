package sync

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoin_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	cases := []struct{ name, path string }{
		{"dotdot", "../escape.txt"},
		{"dotdot_deep", "a/../../escape.txt"},
		{"dotdot_trailing", "a/b/.."},
		{"null_byte", "docs/a\x00.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := safeJoin(root, tc.path)
			if err == nil {
				t.Errorf("expected error for %q, got nil", tc.path)
			}
			if _, ok := err.(*UnsafePathError); !ok {
				t.Errorf("expected *UnsafePathError, got %T", err)
			}
		})
	}
}

// The S2 wire format uses leading slashes ("/docs/a.txt") to mean
// "docs/a.txt relative to the token's scope root". safeJoin strips the
// leading slash before validation, so these should succeed as normal
// relative paths — NOT be rejected as absolute.
func TestSafeJoin_LeadingSlashIsScopeRelative(t *testing.T) {
	root := t.TempDir()
	got, err := safeJoin(root, "/etc/passwd")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	// Must still be under root — joined as "root/etc/passwd".
	if !strings.HasPrefix(got, root) {
		t.Errorf("got %q, want under root %q", got, root)
	}
}

func TestSafeJoin_RejectsEmptyAndRoot(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"", "/"} {
		if _, err := safeJoin(root, p); err == nil {
			t.Errorf("expected error for %q", p)
		}
	}
}

func TestSafeJoin_AcceptsNormalPaths(t *testing.T) {
	root := t.TempDir()
	cases := []struct{ in, wantSuffix string }{
		{"docs/a.txt", "docs/a.txt"},
		{"/docs/a.txt", "docs/a.txt"}, // leading slash stripped
		{"foo/bar/baz.md", "foo/bar/baz.md"},
		{"a.txt", "a.txt"},
	}
	for _, tc := range cases {
		got, err := safeJoin(root, tc.in)
		if err != nil {
			t.Fatalf("safeJoin(%q) error: %v", tc.in, err)
		}
		wantSuffix := filepath.FromSlash(tc.wantSuffix)
		if !strings.HasSuffix(got, wantSuffix) {
			t.Errorf("safeJoin(%q) = %q, want suffix %q", tc.in, got, wantSuffix)
		}
		// Must be under root
		rel, _ := filepath.Rel(root, got)
		if strings.HasPrefix(rel, "..") {
			t.Errorf("safeJoin(%q) escaped root: rel=%q", tc.in, rel)
		}
	}
}

func TestSafeJoin_AllowsSingleDot(t *testing.T) {
	root := t.TempDir()
	// "." as a segment is benign — filepath.Clean collapses it.
	got, err := safeJoin(root, "docs/./a.txt")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.HasSuffix(got, filepath.FromSlash("docs/a.txt")) {
		t.Errorf("got %q", got)
	}
}

func TestSafeRelPrefix_RejectsTraversal(t *testing.T) {
	cases := []string{"../escape", "a/../b", "a/../../b", "foo/\x00bar"}
	for _, tc := range cases {
		if _, err := safeRelPrefix(tc); err == nil {
			t.Errorf("safeRelPrefix(%q) expected error", tc)
		}
	}
}

func TestSafeRelPrefix_ScopeRoot(t *testing.T) {
	for _, in := range []string{"", "/"} {
		got, err := safeRelPrefix(in)
		if err != nil {
			t.Fatalf("safeRelPrefix(%q) error: %v", in, err)
		}
		if got != "" {
			t.Errorf("safeRelPrefix(%q) = %q, want empty (match all)", in, got)
		}
	}
}

func TestSafeRelPrefix_AddsTrailingSlash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"docs", "docs/"},
		{"/docs", "docs/"},
		{"docs/", "docs/"},
		{"docs/sub", "docs/sub/"},
	}
	for _, tc := range cases {
		got, err := safeRelPrefix(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("safeRelPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
