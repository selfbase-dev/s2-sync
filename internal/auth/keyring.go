package auth

import (
	"github.com/zalando/go-keyring"
)

const (
	serviceName = "s2"
	userName    = "default"
)

// keyringStore abstracts the OS secure-storage primitive. The real
// implementation hits the OS keychain; tests swap in an in-memory fake
// via swapKeyring so they don't pollute the developer's keychain or
// require an unlocked login session in CI.
type keyringStore interface {
	get() (string, error)
	set(string) error
	delete() error
}

type osKeyring struct{}

func (osKeyring) get() (string, error) { return keyring.Get(serviceName, userName) }
func (osKeyring) set(v string) error   { return keyring.Set(serviceName, userName, v) }
func (osKeyring) delete() error        { return keyring.Delete(serviceName, userName) }

// keyringHooks is the indirection point for test fakes. Production
// callers go through SetKeyring / GetKeyring / DeleteKeyring below.
var keyringHooks keyringStore = osKeyring{}

// SetKeyring stores the session blob in the system keyring.
func SetKeyring(token string) error { return keyringHooks.set(token) }

// GetKeyring retrieves the session blob from the system keyring.
func GetKeyring() (string, error) { return keyringHooks.get() }

// DeleteKeyring removes the session blob from the system keyring.
func DeleteKeyring() error { return keyringHooks.delete() }
