package engine

import (
	"math"
	"testing"
)

func trackerFrame() []float64 {
	// 3072 samples covers two 60 Hz periods at 48 kHz.
	return make([]float64, 3072)
}

func fillTrackerFrame(x []float64, freq float64) {
	for i := range x {
		t := float64(i) / SampleRate
		x[i] = 0.5*math.Sin(2*math.Pi*freq*t) +
			0.25*math.Sin(2*math.Pi*2*freq*t) +
			0.12*math.Sin(2*math.Pi*3*freq*t)
	}
}

func TestPitchTrackerVoiceFundamentals(t *testing.T) {
	tr := NewPitchTracker(SampleRate)
	frame := trackerFrame()
	for _, want := range []float64{80, 120, 220, 400} {
		fillTrackerFrame(frame, want)
		got := tr.Analyze(frame)
		if !got.Voiced {
			t.Fatalf("f0 %.0f Hz reported unvoiced", want)
		}
		if got.Confidence < 0.9 {
			t.Fatalf("f0 %.0f Hz confidence %.3f, want ≥0.9", want, got.Confidence)
		}
		if err := math.Abs(got.F0-want) / want; err > 0.01 {
			t.Fatalf("f0 %.3f Hz, want %.0f ±1%%", got.F0, want)
		}
	}
}

func TestPitchTrackerOctaveResistant(t *testing.T) {
	// The second harmonic dominates, but the waveform still repeats at
	// 120 Hz. Selecting the strongest spectral line would report 240 Hz.
	tr := NewPitchTracker(SampleRate)
	frame := trackerFrame()
	for i := range frame {
		t_ := float64(i) / SampleRate
		frame[i] = 0.25*math.Sin(2*math.Pi*120*t_) +
			0.45*math.Sin(2*math.Pi*240*t_) +
			0.15*math.Sin(2*math.Pi*360*t_)
	}
	got := tr.Analyze(frame)
	if !got.Voiced {
		t.Fatal("dominant-subharmonic signal reported unvoiced")
	}
	if err := math.Abs(got.F0-120) / 120; err > 0.03 {
		t.Fatalf("f0 %.3f Hz, want ~120 Hz rather than a subharmonic", got.F0)
	}
}

func TestPitchTrackerUnvoicedAndSilence(t *testing.T) {
	tr := NewPitchTracker(SampleRate)
	frame := trackerFrame()
	if got := tr.Analyze(frame); got.Voiced {
		t.Fatalf("silence: got %+v, want unvoiced", got)
	}
	seed := uint32(1)
	for i := range frame {
		seed = seed*1664525 + 1013904223
		frame[i] = float64(int16(seed)) / 32768
	}
	if got := tr.Analyze(frame); got.Voiced {
		t.Fatalf("white noise: got %+v, want unvoiced", got)
	}
}

func TestPitchTrackerChunkLocal(t *testing.T) {
	// Every Analyze call sees exactly one fixed frame. Whether the caller
	// accumulated that frame in 512- or 2048-sample pushes is irrelevant by
	// construction; repeated calls must still return the same estimate.
	tr := NewPitchTracker(SampleRate)
	frame := trackerFrame()
	fillTrackerFrame(frame, 220)
	a := tr.Analyze(frame)
	b := tr.Analyze(frame)
	if a != b {
		t.Fatalf("same frame, different estimate: %+v vs %+v", a, b)
	}
}

func TestPitchTrackerNearHint(t *testing.T) {
	tr := NewPitchTracker(SampleRate)
	frame := trackerFrame()
	// breathy continuation: a 120 Hz tone under noise whose CMND dip
	// (≈2σ²/(A²+2σ²) ≈ 0.28 after decimation) sits between the first-dip
	// threshold and the run's relaxed one
	seed := uint32(7)
	for i := range frame {
		seed = seed*1664525 + 1013904223
		frame[i] = 0.3*math.Sin(2*math.Pi*120*float64(i)/SampleRate) +
			0.46*float64(int16(seed>>16))/32768
	}
	if got := tr.Analyze(frame); got.Voiced {
		t.Fatalf("fixture voiced without hint (%+v); hysteresis untested", got)
	}
	if got := tr.AnalyzeNear(frame, 115); !got.Voiced || math.Abs(got.F0-120)/120 > 0.03 {
		t.Fatalf("with 115 Hz hint: %+v, want voiced ~120 Hz", got)
	}
	// a clearly periodic frame ignores the hint, even at its subharmonic
	fillTrackerFrame(frame, 220)
	if got := tr.AnalyzeNear(frame, 110); math.Abs(got.F0-220)/220 > 0.01 {
		t.Fatalf("220 Hz with 110 Hz hint: %+v, want 220 Hz", got)
	}
}
