package sync

import (
	"fmt"
	"os"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
)

// Open validates localDir as a sync root, ensures .s2ignore, builds an
// authenticated client against endpoint, fetches the caller identity, and
// opens local SQLite state. The caller owns state.Close().
//
// All three call sites (cmd sync, cmd watch, service.SyncService) need the
// same wiring; keep the duplication here, not in each command.
//
// All paths exchanged with the server are relative to the token's
// base_path, which is opaque to s2-sync.
func Open(localDir, endpoint string) (c *client.Client, state *State, err error) {
	info, statErr := os.Stat(localDir)
	if statErr != nil {
		return nil, nil, fmt.Errorf("local directory not found: %w", statErr)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", localDir)
	}

	if err := EnsureIgnoreFile(localDir); err != nil {
		return nil, nil, fmt.Errorf("create .s2ignore: %w", err)
	}

	source, err := auth.NewSource(endpoint)
	if err != nil {
		return nil, nil, err
	}
	c = client.New(endpoint, source)

	ti, err := c.Introspect()
	if err != nil {
		return nil, nil, fmt.Errorf("auth: %w", err)
	}

	identity := Identity{
		Endpoint: endpoint,
		UserID:   ti.UserID,
		TokenID:  ti.TokenID,
	}
	state, err = LoadState(localDir, identity)
	if err != nil {
		return nil, nil, fmt.Errorf("load state: %w", err)
	}

	return c, state, nil
}
