package main

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"runtime"
	"time"

	"github.com/egoist/mygo"

	"voice-morph/engine"
)

// State is everything the UI needs on load.
type State struct {
	Params     engine.Params   `json:"params"`
	Presets    []engine.Preset `json:"presets"`
	EqFreqs    [10]float64     `json:"eqFreqs"`
	SampleRate int             `json:"sampleRate"`
	LatencyMs  float64         `json:"latencyMs"`
	Platform   string          `json:"platform"`
}

// FileResult is the outcome of morphing a whole clip.
type FileResult struct {
	Wav     string  `json:"wav"`     // base64 16-bit PCM WAV, 48 kHz mono
	Seconds float64 `json:"seconds"` // input duration
	InHz    int     `json:"inHz"`    // input sample rate
	OutHz   int     `json:"outHz"`   // output sample rate
	Elapsed float64 `json:"elapsed"` // DSP wall time, ms
}

// Morpher is the bound service: the page calls these methods.
type Morpher struct {
	eng *engine.Engine
}

// GetState returns params, presets and constants for the UI.
func (m *Morpher) GetState() State {
	return State{
		Params:     m.eng.Params(),
		Presets:    engine.Presets,
		EqFreqs:    engine.EqFreqs,
		SampleRate: engine.SampleRate,
		LatencyMs:  m.eng.Latency() * 1000,
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// GetParams returns the current morph parameters.
func (m *Morpher) GetParams() engine.Params { return m.eng.Params() }

// SetParams applies a new parameter set.
func (m *Morpher) SetParams(p engine.Params) { m.eng.SetParams(p) }

// ApplyPreset switches to a named voice and returns its parameters so the
// UI can reflect them on the sliders.
func (m *Morpher) ApplyPreset(id string) (engine.Params, error) {
	for _, p := range engine.Presets {
		if p.ID == id {
			m.eng.SetParams(p.Params)
			return p.Params, nil
		}
	}
	return engine.Params{}, errors.New("unknown preset: " + id)
}

// ResetStream clears streaming state (called when monitoring restarts).
func (m *Morpher) ResetStream() { m.eng.Reset() }

// ProcessChunk morphs one realtime block: base64 little-endian float32
// mono at 48 kHz in, same format out.
func (m *Morpher) ProcessChunk(pcm string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(pcm)
	if err != nil || len(raw)%4 != 0 {
		return "", errors.New("bad pcm payload")
	}
	in := make([]float32, len(raw)/4)
	for i := range in {
		in[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	out := m.eng.Process(in)
	buf := make([]byte, len(out)*4)
	for i, v := range out {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// ProcessFile morphs a whole WAV clip offline and returns a 48 kHz WAV.
func (m *Morpher) ProcessFile(wavB64 string) (FileResult, error) {
	raw, err := base64.StdEncoding.DecodeString(wavB64)
	if err != nil {
		return FileResult{}, err
	}
	pcm, sr, err := engine.DecodeWAV(raw)
	if err != nil {
		return FileResult{}, err
	}
	if sr != engine.SampleRate {
		pcm = resampleTo(pcm, sr, engine.SampleRate)
	}
	secs := float64(len(pcm)) / engine.SampleRate
	t0 := time.Now()
	out := m.eng.Render(pcm)
	return FileResult{
		Wav:     base64.StdEncoding.EncodeToString(engine.EncodeWAV(out, engine.SampleRate)),
		Seconds: secs,
		InHz:    sr, OutHz: engine.SampleRate,
		Elapsed: float64(time.Since(t0)) / 1e6,
	}, nil
}

// resampleTo converts a clip between sample rates with the sinc resampler.
func resampleTo(in []float32, from, to int) []float32 {
	r := engine.NewResampler(float64(to) / float64(from))
	x := make([]float64, len(in))
	for i, v := range in {
		x[i] = float64(v)
	}
	x = append(r.Process(x), r.Flush()...)
	out := make([]float32, len(x))
	for i, v := range x {
		out[i] = float32(v)
	}
	return out
}

func main() {
	mygo.Bind(&Morpher{eng: engine.New()})

	mygo.App.WhenReady(func() {
		mygo.NewWindow(mygo.WindowOptions{
			Title:           "VoiceMorph",
			Width:           1180,
			Height:          760,
			MinWidth:        940,
			MinHeight:       620,
			BackgroundColor: "#faf9f5",
			StateKey:        "main",
			URL:             "/",
		})
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
