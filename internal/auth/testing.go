package auth

// This file exposes a minimal seam so external test packages can wire
// an in-memory keyring without depending on the OS keychain. The
// helpers here are intended only for `*_test.go` callers in other
// packages; production code goes through SetKeyring / GetKeyring /
// DeleteKeyring. They live in a non-_test file because Go's
// `export_test.go` mechanism is invisible to external test packages,
// and routing every external smoke through a separate sub-package
// would require even more public surface to break the import cycle
// (client → auth → would-be-helper).

// inMemoryKeyring is a process-local keyringStore used by tests.
type inMemoryKeyring struct{ data string }

func (f *inMemoryKeyring) get() (string, error) { return f.data, nil }
func (f *inMemoryKeyring) set(v string) error   { f.data = v; return nil }
func (f *inMemoryKeyring) delete() error        { f.data = ""; return nil }

// SwapKeyringForTesting installs an in-memory keyring fake and returns
// a pointer to its backing string plus a cleanup. External tests use
// this to avoid the import cycle they'd hit by reusing the internal
// swapKeyring helper.
func SwapKeyringForTesting() (data *string, restore func()) {
	prev := keyringHooks
	fk := &inMemoryKeyring{}
	keyringHooks = fk
	return &fk.data, func() { keyringHooks = prev }
}

// WriteSessionForTesting persists a Session via the same code path the
// real refresh flow uses. Exposed so external tests can seed a
// near-expired session without re-implementing the serialization rules.
func WriteSessionForTesting(s *Session) error { return writeSession(s) }

// SessionSchemaVersion exposes the current schema version so external
// tests can construct payloads that won't be wiped as legacy.
const SessionSchemaVersion = sessionSchemaVersion
