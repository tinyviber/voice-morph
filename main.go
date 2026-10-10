package main

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"runtime"
	"sync"
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

// streamSpan spaces out the stream IDs of successive monitor epochs:
// epoch N's chunks carry streamID = N*streamSpan + resync index, so a
// resynced stream is always newer than every stream of its own epoch and
// always older than the next epoch's. The JS sender derives its IDs the
// same way from the base ResetStream returns.
const streamSpan = int64(1 << 20)

// staleChunkPrefix tags protocol rejections so the JS sender can tell a
// dead-stream refusal (→ resync onto a fresh stream ID) from a generic
// IPC failure (→ drop the block, keep draining the same stream).
const staleChunkPrefix = "voicemorph: stale chunk"

// Morpher is the bound service: the page calls these methods.
type Morpher struct {
	eng *engine.Engine

	// Chunk protocol state. mygo runs every bound call on its own
	// goroutine and the JS sender abandons an IPC wait on timeout while
	// the Go call keeps running, so ProcessChunk calls can reach eng in
	// any order. mu makes the {curStream, lastSeq} check and the engine
	// work one atomic step, turning reordering into deterministic
	// rejection instead of arbitrary stream-state mutation.
	mu         sync.Mutex
	issued     int64 // monitor epochs handed out by ResetStream
	curStream  int64 // the only stream allowed to mutate DSP state
	lastSeq    int64 // highest seq already processed on curStream (-1: none)
	seqGaps    int64 // seq holes processed over (client-side unsent drops)
	staleDrops int64 // calls rejected by the protocol before any mutation
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

// ResetStream begins a monitor epoch: the engine's streaming state is
// rebuilt and the returned base defines the stream IDs the monitor's
// chunks carry (base + the sender's resync index; epochs are spaced by
// streamSpan). From this point on every chunk on an earlier stream is
// stale and rejected — calls still in flight from a previous monitor can
// no longer mutate state, before the new stream's first block arrives.
func (m *Morpher) ResetStream() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issued++
	m.curStream = m.issued * streamSpan
	m.lastSeq = -1
	m.eng.Reset()
	return m.curStream
}

// ProcessChunk morphs one realtime block: base64 little-endian float32
// mono at 48 kHz in, same format out.
//
// Protocol: every chunk carries its (streamID, seq) and the protocol
// decides each call's fate under mu, atomically with the DSP work, so a
// reordered or late call can never mutate stream state out of turn:
//
//	streamID <  curStream              → stale: rejected, zero mutation
//	streamID == curStream, seq ≤ lastSeq → stale: rejected, zero mutation
//	streamID >  curStream              → new stream adopted: DSP state is
//	                                     rebuilt (the previous stream died
//	                                     ≥ a timeout behind; replaying its
//	                                     alignment would echo stale audio)
//	seq > lastSeq                      → processed, lastSeq = seq; a jump
//	                                     leaves a content hole equivalent
//	                                     to the client's drop-oldest policy
//	                                     and is counted in seqGaps
func (m *Morpher) ProcessChunk(streamID int64, seq int64, pcm string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if streamID < m.curStream || (streamID == m.curStream && seq <= m.lastSeq) {
		m.staleDrops++
		return "", fmt.Errorf("%s (stream %d seq %d; current stream %d lastSeq %d)",
			staleChunkPrefix, streamID, seq, m.curStream, m.lastSeq)
	}
	if streamID > m.curStream {
		m.curStream = streamID
		m.lastSeq = -1
		m.eng.Reset()
	}
	raw, err := base64.StdEncoding.DecodeString(pcm)
	if err != nil || len(raw)%4 != 0 {
		return "", errors.New("bad pcm payload")
	}
	in := make([]float32, len(raw)/4)
	for i := range in {
		in[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	if gap := seq - m.lastSeq - 1; gap > 0 {
		m.seqGaps += gap
	}
	out := m.eng.Process(in)
	m.lastSeq = seq
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
	// Render on a dedicated engine: Render holds the mutex for the whole
	// clip and rebuilds streaming state at the end, so running it on
	// m.eng would stall live ProcessChunk calls and wipe the monitor's
	// buffered stream state.
	eng := engine.New()
	eng.SetParams(m.eng.Params())
	out := eng.Render(pcm)
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
	mygo.Bind(&Morpher{eng: engine.New(), lastSeq: -1})

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
