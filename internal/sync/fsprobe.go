package sync

import (
	"os"
	"path/filepath"
)

// IsCaseInsensitiveFS probes dir by creating a temp file and checking
// whether it's reachable under a different-case filename. Returns true
// on Mac/Windows case-insensitive filesystems, false on Linux and on
// case-sensitive macOS APFS / WSL mounts.
//
// If probing fails (permission, etc.) returns false — treating FS as
// case-sensitive means we don't over-filter, and walk's own collision
// detection still catches the actual local-collision scenarios that
// would break sync.
func IsCaseInsensitiveFS(dir string) bool {
	f, err := os.CreateTemp(dir, ".s2caseprobe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	base := filepath.Base(name)
	flipped := flipCase(base)
	if flipped == base {
		return false
	}
	altPath := filepath.Join(filepath.Dir(name), flipped)
	_, err = os.Stat(altPath)
	return err == nil
}

func flipCase(s string) string {
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r >= 'a' && r <= 'z':
			runes[i] = r - ('a' - 'A')
			return string(runes)
		case r >= 'A' && r <= 'Z':
			runes[i] = r + ('a' - 'A')
			return string(runes)
		}
	}
	return s
}
