package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// HashBytes returns a lowercase hex SHA-256 digest of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashFile returns a SHA-256 digest of the file at path.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}
