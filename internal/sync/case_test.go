package sync

import (
	"sort"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

func TestNormalizePath(t *testing.T) {
	// NFD: "a" + U+0300 (combining grave) — decomposed form of "à"
	nfdA := "à.txt"
	// NFC: U+00E0 ("à") — composed form
	nfcA := "à.txt"

	tests := []struct {
		in, out string
	}{
		{"", ""},
		{"/foo", "foo"},
		{"foo/bar", "foo/bar"},
		{nfdA, nfcA},
		{nfcA, nfcA},
		{"File.TXT", "File.TXT"},
	}
	for _, tt := range tests {
		got := NormalizePath(tt.in)
		if got != tt.out {
			t.Errorf("NormalizePath(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestNormalizePath_Idempotent(t *testing.T) {
	// NFC(NFC(x)) == NFC(x) — critical for stable re-sync
	inputs := []string{
		"file.txt",
		"/ファイル.txt",
		"à.txt",
		"à.txt",
		"A/B/c",
	}
	for _, p := range inputs {
		once := NormalizePath(p)
		twice := NormalizePath(once)
		if once != twice {
			t.Errorf("NormalizePath not idempotent: %q → %q → %q", p, once, twice)
		}
	}
}

func TestFoldKey_CaseInsensitive(t *testing.T) {
	tests := []struct {
		a, b     string
		shouldEq bool
	}{
		{"file.txt", "File.txt", true},
		{"File.txt", "FILE.TXT", true},
		{"file.txt", "other.txt", false},
		// NFD/NFC difference hidden by NFC pass
		{"à.txt", "à.txt", true},
		// Mixed case + NFC
		{"DIR/File.TXT", "dir/file.txt", true},
	}
	for _, tt := range tests {
		eq := FoldKey(tt.a) == FoldKey(tt.b)
		if eq != tt.shouldEq {
			t.Errorf("FoldKey(%q)==FoldKey(%q): got %v, want %v (keys: %q vs %q)",
				tt.a, tt.b, eq, tt.shouldEq, FoldKey(tt.a), FoldKey(tt.b))
		}
	}
}

func TestDetectCollisions_Groups(t *testing.T) {
	got := DetectCollisions([]string{"file.txt", "File.txt", "other.txt", "OTHER.TXT"})
	if len(got) != 2 {
		t.Errorf("expected 2 groups, got %d: %+v", len(got), got)
	}
	for k, paths := range got {
		if len(paths) != 2 {
			t.Errorf("group %q has %d members, want 2", k, len(paths))
		}
	}
}

func TestDetectCollisions_SortedDeterministic(t *testing.T) {
	input := []string{"File.txt", "file.txt", "FILE.TXT"}
	a := DetectCollisions(input)
	b := DetectCollisions(input)
	for k := range a {
		if !sort.StringsAreSorted(a[k]) {
			t.Errorf("group %q not sorted: %v", k, a[k])
		}
		if len(a[k]) != len(b[k]) {
			t.Fatalf("non-deterministic length for %q", k)
		}
		for i := range a[k] {
			if a[k][i] != b[k][i] {
				t.Errorf("non-deterministic order at %q[%d]: %q vs %q", k, i, a[k][i], b[k][i])
			}
		}
	}
}

func TestDetectCollisions_NoCollision(t *testing.T) {
	got := DetectCollisions([]string{"a.txt", "b.txt", "c.txt"})
	for k, paths := range got {
		if len(paths) != 1 {
			t.Errorf("unexpected collision for %q: %v", k, paths)
		}
	}
}

func TestDetectMovePairs_CaseOnly(t *testing.T) {
	archive := map[string]string{"file.txt": "hash1", "other.txt": "hash2"}
	local := map[string]string{"File.txt": "hash1", "other.txt": "hash2"}
	pairs := DetectMovePairs(local, archive)
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d: %+v", len(pairs), pairs)
	}
	if pairs[0].From != "file.txt" || pairs[0].To != "File.txt" {
		t.Errorf("unexpected pair: %+v", pairs[0])
	}
	if pairs[0].Hash != "hash1" {
		t.Errorf("hash mismatch: got %q", pairs[0].Hash)
	}
}

func TestDetectMovePairs_NFDToNFCSamePath(t *testing.T) {
	// After walk's NFC normalization, NFD and NFC versions of the same
	// path collapse to the same key — no rename to detect.
	archive := map[string]string{"à.txt": "h"}
	local := map[string]string{"à.txt": "h"}
	pairs := DetectMovePairs(local, archive)
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs (same NFC path), got %d: %+v", len(pairs), pairs)
	}
}

func TestDetectMovePairs_HashMismatch(t *testing.T) {
	archive := map[string]string{"file.txt": "hash1"}
	local := map[string]string{"File.txt": "hash2"}
	pairs := DetectMovePairs(local, archive)
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs (hash mismatch → not case-only rename), got %d", len(pairs))
	}
}

func TestDetectMovePairs_AmbiguousArchive(t *testing.T) {
	// 2 archive entries fold to same key — ambiguous, don't pair
	archive := map[string]string{"a.txt": "h", "A.txt": "h"}
	local := map[string]string{"b.txt": "h"}
	pairs := DetectMovePairs(local, archive)
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs (ambiguous archive), got %d: %+v", len(pairs), pairs)
	}
}

func TestDetectMovePairs_AmbiguousLocal(t *testing.T) {
	archive := map[string]string{"b.txt": "h"}
	local := map[string]string{"a.txt": "h", "A.txt": "h"}
	pairs := DetectMovePairs(local, archive)
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs (ambiguous local), got %d: %+v", len(pairs), pairs)
	}
}

func TestDetectMovePairs_SamePath(t *testing.T) {
	archive := map[string]string{"file.txt": "h"}
	local := map[string]string{"file.txt": "h"}
	pairs := DetectMovePairs(local, archive)
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs (same path is not a rename), got %d", len(pairs))
	}
}

func TestNormalizeRemoteMap_CaseSensitive(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"File.txt": {Hash: "h1"},
		"file.txt": {Hash: "h2"},
	}
	filtered, collisions := NormalizeRemoteMap(remote, false)
	if len(filtered) != 2 {
		t.Errorf("expected 2 files on case-sensitive FS, got %d", len(filtered))
	}
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions on case-sensitive FS, got %d", len(collisions))
	}
}

func TestNormalizeRemoteMap_CaseInsensitive(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"File.txt": {Hash: "h1"},
		"file.txt": {Hash: "h2"},
		"other":    {Hash: "h3"},
	}
	filtered, collisions := NormalizeRemoteMap(remote, true)
	if len(filtered) != 2 {
		t.Errorf("expected 2 files (1 winner + other), got %d: %+v", len(filtered), filtered)
	}
	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	if len(collisions[0].Paths) != 2 {
		t.Errorf("collision group should have 2 paths, got %d: %v", len(collisions[0].Paths), collisions[0].Paths)
	}
	// Lex-first wins: "File.txt" < "file.txt"
	if _, ok := filtered["File.txt"]; !ok {
		t.Errorf("lex-first winner File.txt should be in filtered, got keys: %v", keysOf(filtered))
	}
}

func TestNormalizeRemoteMap_NFC(t *testing.T) {
	// Server sends a path that would be NFC-normalized (defensive — server stores NFC)
	remote := map[string]types.RemoteFile{
		"/à.txt": {Hash: "h"}, // with leading slash
	}
	filtered, _ := NormalizeRemoteMap(remote, false)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 file, got %d", len(filtered))
	}
	// Key should be canonical: no leading slash, NFC
	if _, ok := filtered["à.txt"]; !ok {
		t.Errorf("expected canonical key 'à.txt', got keys: %v", keysOf(filtered))
	}
}

func TestMergeCaseOnlyRenames_Merges(t *testing.T) {
	local := map[string]types.LocalFile{
		"File.txt":  {Hash: "h1"},
		"other.txt": {Hash: "h2"},
	}
	archive := map[string]types.FileState{
		"file.txt":  {LocalHash: "h1"},
		"other.txt": {LocalHash: "h2"},
	}
	plans := []types.SyncPlan{
		{Path: "file.txt", Action: types.DeleteRemote},
		{Path: "File.txt", Action: types.Push, Hash: "h1"},
		{Path: "keep.txt", Action: types.Push},
	}
	out := MergeCaseOnlyRenames(plans, local, archive)
	// Expect: Move(file.txt → File.txt) + keep.txt
	if len(out) != 2 {
		t.Fatalf("expected 2 plans after merge, got %d: %+v", len(out), out)
	}
	var found bool
	for _, p := range out {
		if p.Action == types.Move && p.From == "file.txt" && p.Path == "File.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Move(file.txt → File.txt) in output: %+v", out)
	}
}

func TestMergeCaseOnlyRenames_NoMerge_HashDiffers(t *testing.T) {
	local := map[string]types.LocalFile{
		"File.txt": {Hash: "h2"},
	}
	archive := map[string]types.FileState{
		"file.txt": {LocalHash: "h1"}, // different hash → not a case-only rename
	}
	plans := []types.SyncPlan{
		{Path: "file.txt", Action: types.DeleteRemote},
		{Path: "File.txt", Action: types.Push, Hash: "h2"},
	}
	out := MergeCaseOnlyRenames(plans, local, archive)
	if len(out) != 2 {
		t.Errorf("expected 2 plans (unchanged), got %d: %+v", len(out), out)
	}
	for _, p := range out {
		if p.Action == types.Move {
			t.Errorf("should not have produced Move: %+v", p)
		}
	}
}

func TestNeutralize_PlanVsPlan_CaseInsensitive(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "File.txt", Action: types.Push},
		{Path: "file.txt", Action: types.Pull},
		{Path: "other.txt", Action: types.Push},
	}
	out := NeutralizeLocalRemoteCaseCollisions(plans, nil, nil, true)
	var skipCount int
	for _, p := range out {
		if p.Action == types.SkipCaseConflict {
			skipCount++
		}
	}
	if skipCount != 2 {
		t.Errorf("expected 2 SkipCaseConflict plans (File.txt + file.txt), got %d: %+v", skipCount, out)
	}
}

// Regression: Pull on a path whose FoldKey collides with an existing
// LOCAL file (not in plans) must be neutralized — otherwise the Pull
// overwrites the local's inode on case-insensitive FS.
func TestNeutralize_PullVsExistingLocal(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "File.txt", Action: types.Pull},
	}
	local := map[string]types.LocalFile{"file.txt": {Hash: "h"}}
	out := NeutralizeLocalRemoteCaseCollisions(plans, local, nil, true)
	if out[0].Action != types.SkipCaseConflict {
		t.Errorf("expected SkipCaseConflict, got %v", out[0].Action)
	}
}

// Push for an exact-match existing local path is NOT a collision.
func TestNeutralize_PushOfExactMatch_Allowed(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "file.txt", Action: types.Push},
	}
	local := map[string]types.LocalFile{"file.txt": {Hash: "h"}}
	out := NeutralizeLocalRemoteCaseCollisions(plans, local, nil, true)
	if out[0].Action != types.Push {
		t.Errorf("exact-match push should stay Push, got %v", out[0].Action)
	}
}

// Move for a path whose FoldKey collides with the source (because we
// are renaming into that slot) must NOT be neutralized.
func TestNeutralize_MoveIsAllowed(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "File.txt", From: "file.txt", Action: types.Move, Hash: "h"},
	}
	local := map[string]types.LocalFile{"File.txt": {Hash: "h"}}
	archive := map[string]types.FileState{"file.txt": {LocalHash: "h"}}
	out := NeutralizeLocalRemoteCaseCollisions(plans, local, archive, true)
	if out[0].Action != types.Move {
		t.Errorf("Move should not be neutralized, got %v", out[0].Action)
	}
}

func TestNeutralize_CaseSensitive_NoOp(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "File.txt", Action: types.Push},
		{Path: "file.txt", Action: types.Pull},
	}
	out := NeutralizeLocalRemoteCaseCollisions(plans, nil, nil, false)
	if out[0].Action != types.Push || out[1].Action != types.Pull {
		t.Errorf("expected no changes on case-sensitive FS: %+v", out)
	}
}

func TestUniqSorted(t *testing.T) {
	got := uniqSorted([]string{"b", "a", "b", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDiffSortedStrings(t *testing.T) {
	added, resolved := diffSortedStrings([]string{"a", "b", "c"}, []string{"b", "c", "d"})
	if len(added) != 1 || added[0] != "d" {
		t.Errorf("added = %v, want [d]", added)
	}
	if len(resolved) != 1 || resolved[0] != "a" {
		t.Errorf("resolved = %v, want [a]", resolved)
	}
}

func TestEncodeParseCollisionKeys_RoundTrip(t *testing.T) {
	in := []string{"a", "b/file.txt", "ファイル.txt"}
	encoded := encodeCollisionKeys(in)
	got := parseCollisionKeys(encoded)
	if len(got) != len(in) {
		t.Fatalf("lost entries: got %v, want %v", got, in)
	}
	// Must be sorted
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("not sorted: %v", got)
		}
	}
}

func TestEncodeParseCollisionKeys_Empty(t *testing.T) {
	if encodeCollisionKeys(nil) != "" {
		t.Error("nil should encode to empty")
	}
	if parseCollisionKeys("") != nil {
		t.Error("empty should parse to nil")
	}
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestDetectMovePairs_DeterministicOrder(t *testing.T) {
	archive := map[string]string{"a.txt": "h1", "b.txt": "h2"}
	local := map[string]string{"A.txt": "h1", "B.txt": "h2"}
	p1 := DetectMovePairs(local, archive)
	p2 := DetectMovePairs(local, archive)
	if len(p1) != 2 || len(p2) != 2 {
		t.Fatalf("expected 2 pairs each, got %d and %d", len(p1), len(p2))
	}
	for i := range p1 {
		if p1[i] != p2[i] {
			t.Errorf("non-deterministic pair at %d: %+v vs %+v", i, p1[i], p2[i])
		}
	}
	if p1[0].From > p1[1].From {
		t.Errorf("pairs not sorted by From: %v", p1)
	}
}
