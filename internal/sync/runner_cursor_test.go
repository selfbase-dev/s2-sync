// runner_cursor_test.go — drives RunIncrementalSync against an
// httptest server to verify the cursor policy:
//
//   - revision-fetch 404 events advance the cursor (the bug fix for the
//     2026-05-10 incident: a permanent 404 must not become a poison
//     pill that holds the cursor forever)
//   - retryable errors (5xx, network) still hold the cursor for next
//     attempt

package sync

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
)

func newRunnerTestState(t *testing.T) *State {
	t.Helper()
	st, err := LoadState(t.TempDir(), Identity{Endpoint: "https://t.local", UserID: "u", TokenID: "tok"})
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRunIncrementalSync_CursorAdvances_WhenPullEvents_Are404(t *testing.T) {
	// Server returns one put event for gone.txt at rev-stale.
	// Both the revision fetch and the path fetch return 404 → event
	// must be bucketed as RevisionSkipped, NOT Errors → cursor advances
	// to "cursor-next".
	pollHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/changes":
			pollHits++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"changes": []map[string]any{{
					"seq":         1,
					"action":      "put",
					"path_after":  "gone.txt",
					"is_dir":      false,
					"revision_id": "rev-stale",
					"hash":        "h-stale",
				}},
				"next_cursor": "cursor-next",
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/revisions/"),
			strings.HasPrefix(r.URL.Path, "/api/v1/files/"):
			// Both 404 → permanent gone.
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := client.New(srv.URL, auth.NewStaticSource("s2_test"))

	st := newRunnerTestState(t)
	st.Cursor = "cursor-prev"

	err := RunIncrementalSync(c, t.TempDir(), st, SyncOptions{})
	if err != nil {
		t.Fatalf("RunIncrementalSync: %v", err)
	}
	if st.Cursor != "cursor-next" {
		t.Errorf("cursor = %q, want cursor-next (404 must advance, not poison)", st.Cursor)
	}
	if got := st.SkippedRevisionCount("gone.txt|rev-stale"); got != 1 {
		t.Errorf("skip counter = %d, want 1", got)
	}
	if pollHits != 1 {
		t.Errorf("PollChanges hits = %d, want 1", pollHits)
	}
}

func TestRunIncrementalSync_CursorHeld_WhenPullEvent_Is5xx(t *testing.T) {
	// One put event whose revision fetch returns 500 (retryable).
	// Cursor must NOT advance — the next sync retries from the same
	// position.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/changes":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"changes": []map[string]any{{
					"seq":         2,
					"action":      "put",
					"path_after":  "flaky.txt",
					"is_dir":      false,
					"revision_id": "rev-flake",
					"hash":        "h-flake",
				}},
				"next_cursor": "cursor-after-flake",
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/revisions/"),
			strings.HasPrefix(r.URL.Path, "/api/v1/files/"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := client.New(srv.URL, auth.NewStaticSource("s2_test"))

	st := newRunnerTestState(t)
	st.Cursor = "cursor-stuck"

	_ = RunIncrementalSync(c, t.TempDir(), st, SyncOptions{})
	if st.Cursor != "cursor-stuck" {
		t.Errorf("cursor = %q, want cursor-stuck (5xx must hold cursor for retry)", st.Cursor)
	}
}

func TestRunIncrementalSync_DegenerateLoopWarn_AfterRepeatedSkips(t *testing.T) {
	// Same 404 event served on each poll. After degenerateSkipThreshold
	// syncs, the per-event warn fires.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/changes":
			w.Header().Set("Content-Type", "application/json")
			// Always include the same event so each sync attempts it.
			json.NewEncoder(w).Encode(map[string]any{
				"changes": []map[string]any{{
					"seq":         3,
					"action":      "put",
					"path_after":  "loop.txt",
					"is_dir":      false,
					"revision_id": "rev-loop",
					"hash":        "h-loop",
				}},
				"next_cursor": fmt.Sprintf("cursor-%s", r.URL.RawQuery),
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/revisions/"),
			strings.HasPrefix(r.URL.Path, "/api/v1/files/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := client.New(srv.URL, auth.NewStaticSource("s2_test"))

	st := newRunnerTestState(t)
	logger, cap := newCaptureLog()

	for i := 1; i <= degenerateSkipThreshold; i++ {
		if err := RunIncrementalSync(c, t.TempDir(), st, SyncOptions{Logger: logger}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	warns := cap.find("sync.skip_degenerate")
	if len(warns) == 0 {
		t.Fatalf("no degenerate warn after %d repeated skips", degenerateSkipThreshold)
	}
	last := warns[len(warns)-1]
	if last["path"] != "loop.txt" || last["revision_id"] != "rev-loop" {
		t.Errorf("warn fields = %+v, want path=loop.txt revision_id=rev-loop", last)
	}
	if last["consecutive_skips"] != float64(degenerateSkipThreshold) {
		t.Errorf("consecutive_skips = %v, want %d", last["consecutive_skips"], degenerateSkipThreshold)
	}
}
