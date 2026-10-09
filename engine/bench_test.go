package engine

import (
	"math"
	"testing"
)

// benchClip returns a periodic multi-harmonic signal (~1 s at 48 kHz,
// length rounded down to a whole number of 2048-sample chunks). A
// periodic clip gives WSOLA's correlation search real structure to
// lock onto, matching live input better than noise.
func benchClip() ([]float64, int) {
	const chunk = 2048
	n := chunk * (SampleRate / chunk) // 23 chunks = 47104 samples
	x := make([]float64, n)
	for i := range x {
		t := float64(i) / SampleRate
		x[i] = 0.4*math.Sin(2*math.Pi*180*t) +
			0.2*math.Sin(2*math.Pi*360*t) +
			0.1*math.Sin(2*math.Pi*900*t) +
			0.05*math.Sin(2*math.Pi*2400*t)
	}
	return x, chunk
}

// benchClip32 is the float32 view of benchClip for Engine.Process.
func benchClip32(x []float64) []float32 {
	y := make([]float32, len(x))
	for i, v := range x {
		y[i] = float32(v)
	}
	return y
}

// BenchmarkFFT2048 times the forward transform at the formant stage's
// frame size — the dominant math.Sin/Cos consumer.
func BenchmarkFFT2048(b *testing.B) {
	b.ReportAllocs()
	buf := make([]complex128, 2048)
	for i := range buf {
		buf[i] = complex(float64(i%97)*0.01, float64(i%53)*0.02)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fft(buf, false)
	}
}

// BenchmarkFormantProcess times one streaming 2048-sample Process call
// (~1 s clip cycled). warp=2^0.3 matches an Engine timbre of 0.3.
func BenchmarkFormantProcess(b *testing.B) {
	b.ReportAllocs()
	x, chunk := benchClip()
	f := NewFormant(math.Pow(2, 0.3), 1.0, SampleRate)
	nchunks := len(x) / chunk
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := (i % nchunks) * chunk
		f.Process(x[off : off+chunk])
	}
}

// BenchmarkWSOLAProcess times one streaming Process call at
// alpha = 1/1.4 (pitch ≈ +0.485 through the resample pair).
func BenchmarkWSOLAProcess(b *testing.B) {
	b.ReportAllocs()
	x, chunk := benchClip()
	w := NewWSOLA(1.0/1.4, 2048, 512, 256, SampleRate)
	nchunks := len(x) / chunk
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := (i % nchunks) * chunk
		w.Process(x[off : off+chunk])
	}
}

// BenchmarkEngineProcess times one Engine.Process call on a
// 2048-float32 block at pitch=0.5, timbre=0.3.
func BenchmarkEngineProcess(b *testing.B) {
	b.ReportAllocs()
	x64, chunk := benchClip()
	x := benchClip32(x64)
	e := New()
	p := DefaultParams
	p.Pitch, p.Timbre = 0.5, 0.3
	e.SetParams(p)
	nchunks := len(x) / chunk
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := (i % nchunks) * chunk
		e.Process(x[off : off+chunk])
	}
}
