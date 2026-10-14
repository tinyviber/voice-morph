package engine

import "math"

// SpeechChain is the speech-aware path: TD-PSOLA moves the excitation
// period, then a speech-shaped envelope warp moves the formants. Both
// stages run on the input clock, so its content offset is 0 — unlike the
// resample+WSOLA path it never needs a fitted onset prior. A stage with
// identity settings is left out (nil) rather than resynthesizing.
type SpeechChain struct {
	p *SpeechPitch
	f *Formant
}

// NewSpeechChain builds the speech path for normalized params p. The
// formant stage composes the global timbre amount with the vocal-tract
// shape term; it is not compensated for pitch because PSOLA, unlike
// resampling, leaves the envelope in place.
func NewSpeechChain(p Params, sr float64) *SpeechChain {
	rate := int(sr)
	c := &SpeechChain{}
	if !pitchNeutral(p) {
		c.p = NewSpeechPitch(p, rate)
	}
	warp, shape := math.Exp2(p.Timbre*p.Strength), p.FormantShape*p.Strength
	if warp != 1 || shape != 0 {
		c.f = NewSpeechFormant(warp, shape, rate)
	}
	return c
}

// Transparent reports whether the chain does no work; the engine then
// uses its nil (dry) chain.
func (c *SpeechChain) Transparent() bool { return c.p == nil && c.f == nil }

func (c *SpeechChain) Process(x []float64) []float64 {
	if c.p != nil {
		x = c.p.Process(x)
	}
	if c.f != nil {
		x = c.f.Process(x)
	}
	return x
}

func (c *SpeechChain) Flush() []float64 {
	var x []float64
	if c.p != nil {
		x = c.p.Flush()
	}
	if c.f != nil {
		x = c.f.Process(x)
		x = append(x, c.f.Flush()...)
	}
	return x
}

// Offset is 0: both stages emit sample k for input content k.
func (c *SpeechChain) Offset() int { return 0 }
