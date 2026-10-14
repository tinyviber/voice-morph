package dubbing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UserConfig is persisted at <UserConfigDir>/voice-morph/dubbing.json.
// CLI and GUI share it.
type UserConfig struct {
	Version    int    `json:"version"`
	ModelDir   string `json:"model_dir,omitempty"`
	VADDir     string `json:"vad_dir,omitempty"`
	PythonPath string `json:"python_path,omitempty"`
	Language   string `json:"language,omitempty"`
}

func defaultConfigPath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "voice-morph", "dubbing.json"), nil
}

// LoadUserConfig reads the config at path ("" = default location).
// A missing file is not an error.
func LoadUserConfig(path string) (*UserConfig, error) {
	if path == "" {
		var err error
		if path, err = defaultConfigPath(); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &UserConfig{Version: SchemaVersion, Language: "zh"}, nil
		}
		return nil, err
	}
	var c UserConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid config json: %w", err)
	}
	if c.Language == "" {
		c.Language = "zh"
	}
	return &c, nil
}

func (c *UserConfig) Save(path string) error {
	if path == "" {
		var err error
		if path, err = defaultConfigPath(); err != nil {
			return err
		}
	}
	c.Version = SchemaVersion
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// NormalizePath resolves to an absolute, symlink-free path and
// requires it to exist and be a directory.
func NormalizePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	res, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(res)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", res)
	}
	return res, nil
}
