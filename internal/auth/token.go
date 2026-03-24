package auth

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// LoadToken returns the S2 API token from (in priority order):
// 1. --token flag / S2_TOKEN env var (via viper)
// 2. System keyring
// 3. Config file token field
func LoadToken() (string, error) {
	// 1. Flag or env var
	if t := viper.GetString("token"); t != "" {
		if !strings.HasPrefix(t, "s2_") {
			return "", fmt.Errorf("invalid token format: must start with s2_")
		}
		return t, nil
	}

	// 2. Keyring
	t, err := GetKeyring()
	if err == nil && t != "" {
		return t, nil
	}

	return "", fmt.Errorf("no token found. Run 's2 login' or set S2_TOKEN")
}
