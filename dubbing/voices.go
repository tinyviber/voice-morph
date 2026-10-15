package dubbing

import (
	"encoding/json"
	"fmt"
	"os"

	"voice-morph/engine"
	"voice-morph/evaluation"
)

// customParams is the raw `params` object in a voices file; it is parsed
// via evaluation.ParseParams so omitted fields fall back to the engine
// defaults rather than decoding to zero (gain=0 would silence output).
type customParams = json.RawMessage

// VoiceBackendDSP is the only rendering backend implemented today: the
// in-repo DSP engine. Other backends (e.g. a neural VC adapter) may be
// declared in voices files for forward compatibility but resolve to an
// error until implemented.
const VoiceBackendDSP = "dsp"

// VoiceProfile maps one role to a voice. Exactly one of Preset or Params
// must be set. Gain is an extra linear output gain applied after the
// engine render (0 or 1 means none).
type VoiceProfile struct {
	Backend string       `json:"backend"`          // "dsp"
	Preset  string       `json:"preset,omitempty"` // engine.Presets id
	Params  customParams `json:"params,omitempty"` // full custom params object
	Gain    float64      `json:"gain,omitempty"`   // post-render gain, default 1
}

// Voices is the versioned role→voice map (voices.json), persisted per
// project so voice casting is stable across sessions.
type Voices struct {
	Version int                    `json:"version"`
	Voices  map[string]VoiceProfile `json:"voices"`
	// Default, when present, applies to roles not listed in Voices.
	// Unmatched segments still never inherit it silently — they must be
	// assigned a role during review first.
	Default *VoiceProfile `json:"default,omitempty"`
}

// ParseVoices decodes and validates a voices document.
func ParseVoices(data []byte) (*Voices, error) {
	var v Voices
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("voices: invalid JSON: %w", err)
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return &v, nil
}

// LoadVoices reads and validates a voices file.
func LoadVoices(path string) (*Voices, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read voices %s: %w", path, err)
	}
	return ParseVoices(b)
}

// Validate checks every profile is well-formed and resolvable.
func (v *Voices) Validate() error {
	if v.Version < 1 || v.Version > SchemaVersion {
		return fmt.Errorf("voices: unsupported version %d (this build reads 1..%d)", v.Version, SchemaVersion)
	}
	if len(v.Voices) == 0 && v.Default == nil {
		return fmt.Errorf("voices: empty voice map and no default")
	}
	for role, p := range v.Voices {
		if _, err := resolveProfile(p); err != nil {
			return fmt.Errorf("voices: role %q: %w", role, err)
		}
	}
	if v.Default != nil {
		if _, err := resolveProfile(*v.Default); err != nil {
			return fmt.Errorf("voices: default: %w", err)
		}
	}
	return nil
}

// resolveProfile turns a profile into engine params.
func resolveProfile(p VoiceProfile) (engine.Params, error) {
	if p.Backend != VoiceBackendDSP {
		return engine.Params{}, fmt.Errorf("unsupported backend %q (only %q is implemented)", p.Backend, VoiceBackendDSP)
	}
	var params engine.Params
	switch {
	case p.Preset != "" && len(p.Params) > 0:
		return engine.Params{}, fmt.Errorf("set either preset or params, not both")
	case p.Preset != "":
		pr, ok := evaluation.PresetByID(p.Preset)
		if !ok {
			return engine.Params{}, fmt.Errorf("unknown preset %q", p.Preset)
		}
		params = pr.Params
	case len(p.Params) > 0:
		cp, err := evaluation.ParseParams(p.Params)
		if err != nil {
			return engine.Params{}, err
		}
		params = cp
	default:
		return engine.Params{}, fmt.Errorf("profile needs preset or params")
	}
	if p.Gain != 0 && p.Gain != 1 {
		params.Gain *= p.Gain
	}
	return params, nil
}

// ParamsFor resolves the engine parameters for a role id.
func (v *Voices) ParamsFor(roleID string) (engine.Params, error) {
	if p, ok := v.Voices[roleID]; ok {
		return resolveProfile(p)
	}
	if v.Default != nil {
		return resolveProfile(*v.Default)
	}
	return engine.Params{}, fmt.Errorf("no voice profile for role %q", roleID)
}

// Experimental reports whether a role resolves to a preset flagged
// experimental, so callers can surface the caveat in outputs.
func (v *Voices) Experimental(roleID string) bool {
	p, ok := v.Voices[roleID]
	if !ok || p.Preset == "" {
		return false
	}
	pr, ok := evaluation.PresetByID(p.Preset)
	return ok && pr.Experimental
}
