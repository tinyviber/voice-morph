package dubbing

import (
	"encoding/json"
	"fmt"
	"os"
)

// ScriptLine is one line of the dubbing script produced by the external
// writing agent, in intended reading order.
type ScriptLine struct {
	LineID    string `json:"line_id"`
	SceneID   string `json:"scene_id,omitempty"`
	RoleID    string `json:"role_id"`
	Text      string `json:"text"`
	Direction string `json:"direction,omitempty"`
}

// Script is the versioned script document (script.json).
type Script struct {
	Version int          `json:"version"`
	Lines   []ScriptLine `json:"lines"`
}

// ParseScript decodes and validates a script document.
func ParseScript(data []byte) (*Script, error) {
	var s Script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("script: invalid JSON: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadScript reads and validates a script file.
func LoadScript(path string) (*Script, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read script %s: %w", path, err)
	}
	return ParseScript(b)
}

// Validate checks the script's structural invariants.
func (s *Script) Validate() error {
	if s.Version < 1 || s.Version > SchemaVersion {
		return fmt.Errorf("script: unsupported version %d (this build reads 1..%d)", s.Version, SchemaVersion)
	}
	if len(s.Lines) == 0 {
		return fmt.Errorf("script: no lines")
	}
	seen := make(map[string]bool, len(s.Lines))
	for i, l := range s.Lines {
		if l.LineID == "" {
			return fmt.Errorf("script: line %d has empty line_id", i)
		}
		if seen[l.LineID] {
			return fmt.Errorf("script: duplicate line_id %q", l.LineID)
		}
		seen[l.LineID] = true
		if l.RoleID == "" {
			return fmt.Errorf("script: line %s has empty role_id", l.LineID)
		}
		if l.Text == "" {
			return fmt.Errorf("script: line %s has empty text", l.LineID)
		}
	}
	return nil
}

// Line returns the script line with the given id, or nil.
func (s *Script) Line(id string) *ScriptLine {
	for i := range s.Lines {
		if s.Lines[i].LineID == id {
			return &s.Lines[i]
		}
	}
	return nil
}
