//go:build !darwin

package secrets

import "fmt"

// Read is unavailable off macOS; the Keychain is an Apple API.
func Read(string) (string, error) {
	return "", fmt.Errorf("keychain credentials are only supported on macOS")
}
