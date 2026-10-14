package dubbing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SHA256File returns the hex digest of a file's contents.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteFileAtomic writes via tmp+rename so a crash never leaves a
// truncated artifact behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SessionDir normalizes and creates the session output directory.
func SessionDir(out string) (string, error) {
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("output directory is empty")
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, 0o755)
}
