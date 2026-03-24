package auth

import (
	"github.com/zalando/go-keyring"
)

const (
	serviceName = "s2"
	userName    = "default"
)

// SetKeyring stores the token in the system keyring.
func SetKeyring(token string) error {
	return keyring.Set(serviceName, userName, token)
}

// GetKeyring retrieves the token from the system keyring.
func GetKeyring() (string, error) {
	return keyring.Get(serviceName, userName)
}

// DeleteKeyring removes the token from the system keyring.
func DeleteKeyring() error {
	return keyring.Delete(serviceName, userName)
}
