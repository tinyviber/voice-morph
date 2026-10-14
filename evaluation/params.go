package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"voice-morph/engine"
)

// EvalParams is intentionally the production schema. Keeping one schema
// ensures a custom JSON file renders exactly as the application would.
type EvalParams = engine.Params

func DefaultEvalParams() EvalParams { return engine.DefaultParams }

// ParseParams overlays an object on the engine defaults. Explicit zeros
// (gain=0, pitchRange=0, strength=0) are preserved. Unknown fields,
// non-object roots, trailing values and out-of-range parameters fail
// early rather than silently changing a listening condition.
func ParseParams(data []byte) (EvalParams, error) {
	p := DefaultEvalParams()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return p, errors.New("params JSON must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return DefaultEvalParams(), fmt.Errorf("parse params: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return DefaultEvalParams(), errors.New("parse params: trailing JSON data")
	}
	if err := ValidateParams(p); err != nil {
		return DefaultEvalParams(), err
	}
	return p, nil
}

// ValidateParams rejects bad input instead of relying on engine clamping
// to produce a report with different settings than the user requested.
func ValidateParams(p EvalParams) error {
	for _, v := range []struct {
		name          string
		value, lo, hi float64
	}{
		{"pitch", p.Pitch, -1, 1}, {"timbre", p.Timbre, -1, 1},
		{"strength", p.Strength, 0, 1}, {"gain", p.Gain, 0, 2},
		{"pitchRange", p.PitchRange, 0, 1.5},
		{"targetF0", p.TargetF0, 0, 500},
		{"formantShape", p.FormantShape, -1, 1},
	} {
		if math.IsNaN(v.value) || math.IsInf(v.value, 0) || v.value < v.lo || v.value > v.hi {
			return fmt.Errorf("params %s must be finite in [%g,%g]", v.name, v.lo, v.hi)
		}
	}
	if p.TargetF0 > 0 && p.TargetF0 < 70 {
		return errors.New("params targetF0 must be 0 (auto) or 70..500 Hz")
	}
	for _, bands := range [][10]float64{p.EqPre, p.EqPost} {
		for _, gain := range bands {
			if math.IsNaN(gain) || math.IsInf(gain, 0) || gain < -24 || gain > 24 {
				return errors.New("params EQ gains must be finite in [-24,24] dB")
			}
		}
	}
	return nil
}

// PresetSpec describes a production engine preset.
type PresetSpec = engine.Preset

// Presets returns a copy so callers cannot modify production defaults.
func Presets() []PresetSpec { return append([]PresetSpec(nil), engine.Presets...) }

// PresetByID returns a preset by exact ID.
func PresetByID(id string) (PresetSpec, bool) {
	for _, p := range engine.Presets {
		if p.ID == id {
			return p, true
		}
	}
	return PresetSpec{}, false
}
