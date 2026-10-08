package engine

import (
	"math"
	"testing"
)

func sine(freq, dur float64, sr int) []float32 {
	n := int(dur * float64(sr))
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(0.6 * math.Sin(2*math.Pi*freq*float64(i)/float64(sr)))
	}
	return x
}

// estimateF0 measures fundamental frequency by autocorrelation.
func estimateF0(x []float32, sr int) float64 {
	// skip the first 15% to dodge filter warmup
	start := len(x) / 7
	x = x[start : len(x)-start]
	bestLag, bestV := 0, -1.0
	var energy float64
	for _, v := range x {
		energy += float64(v) * float64(v)
	}
	for lag := sr / 500; lag <= sr/50; lag++ {
		var c float64
		for i := 0; i+lag < len(x); i++ {
			c += float64(x[i]) * float64(x[i+lag])
		}
		c /= energy
		if c > bestV {
			bestV, bestLag = c, lag
		}
	}
	if bestLag == 0 {
		return 0
	}
	return float64(sr) / float64(bestLag)
}

func rms(x []float32) float64 {
	var e float64
	for _, v := range x {
		e += float64(v) * float64(v)
	}
	return math.Sqrt(e / float64(len(x)))
}

func TestPitchShiftSine(t *testing.T) {
	in := sine(220, 1.0, SampleRate)
	for _, pitch := range []float64{-0.5, 0.5, 0.8} {
		e := New()
		p := DefaultParams
		p.Pitch = pitch
		e.SetParams(p)
		out := e.Render(in)
		want := 220 * math.Pow(2, pitch)
		got := estimateF0(out, SampleRate)
		if math.Abs(got-want)/want > 0.08 {
			t.Errorf("pitch %+v: f0 = %.1f Hz, want ~%.1f", pitch, got, want)
		}
		if got := math.Abs(float64(len(out))/float64(len(in)) - 1); got > 0.1 {
			t.Errorf("pitch %+v: length changed by %.0f%%", pitch, got*100)
		}
	}
}

func TestPitchIdentity(t *testing.T) {
	in := sine(220, 0.5, SampleRate)
	e := New()
	out := e.Render(in)
	if math.Abs(rms(out)-rms(in))/rms(in) > 0.2 {
		t.Errorf("identity render: rms %.3f in, %.3f out", rms(in), rms(out))
	}
}

// envelopePeak finds the frequency of the strongest spectral-envelope peak.
func envelopePeak(x []float32, sr int) float64 {
	n := 8192
	if len(x) < n {
		n = len(x)
	}
	x = x[len(x)/2-n/2 : len(x)/2+n/2]
	buf := make([]complex128, n)
	win := hann(n)
	for i := range x {
		buf[i] = complex(float64(x[i])*win[i], 0)
	}
	fft(buf, false)
	lm := make([]float64, n)
	for i := range lm {
		lm[i] = math.Log(math.Max(cmplxAbs(buf[i]), 1e-8))
	}
	// cepstral lifter → smooth envelope
	cb := make([]complex128, n)
	for i := range cb {
		cb[i] = complex(lm[i], 0)
	}
	fft(cb, true)
	for i := range cb {
		if i > 96 && i < n-96 {
			cb[i] = 0
		}
	}
	fft(cb, false)
	peak, pv := 0, -1e9
	for i := 100; i < n/2; i++ { // skip DC region
		if real(cb[i]) > pv {
			pv, peak = real(cb[i]), i
		}
	}
	return float64(peak) * float64(sr) / float64(n)
}

func TestFormantShift(t *testing.T) {
	// broadband signal with a spectral peak near 900 Hz: filtered noise
	// via a strong 900 Hz resonance built from decaying cosines
	in := make([]float32, SampleRate/2)
	for i := range in {
		t_ := float64(i) / SampleRate
		in[i] = float32(0.4*math.Exp(-t_*3)*math.Cos(2*math.Pi*900*t_) +
			0.05*math.Cos(2*math.Pi*300*t_))
	}
	before := envelopePeak(in, SampleRate)

	e := New()
	p := DefaultParams
	p.Timbre = 0.5 // warp = 2^0.5 ≈ 1.41
	e.SetParams(p)
	out := e.Render(in)
	after := envelopePeak(out, SampleRate)
	if after/before < 1.2 {
		t.Errorf("formant peak moved %.0f→%.0f Hz, want ≥1.2x", before, after)
	}
}

func TestEQ(t *testing.T) {
	// small input so the +12 dB boost stays well under the soft-clip knee
	in := make([]float32, SampleRate/2)
	for i := range in {
		in[i] = float32(0.05 * math.Sin(2*math.Pi*1000*float64(i)/SampleRate))
	}
	var gains [10]float64
	gains[5] = 12 // +12 dB at 1 kHz
	e := New()
	p := DefaultParams
	p.EqPre = gains
	e.SetParams(p)
	out := e.Render(in)
	got := rms(out) / rms(in)
	if got < 3 || got > 5 {
		t.Errorf("+12 dB at 1 kHz: amplitude ratio %.2f, want ~4.0", got)
	}
}

func TestWAVRoundTrip(t *testing.T) {
	in := sine(440, 0.1, SampleRate)
	data := EncodeWAV(in, SampleRate)
	got, sr, err := DecodeWAV(data)
	if err != nil {
		t.Fatal(err)
	}
	if sr != SampleRate || len(got) != len(in) {
		t.Fatalf("roundtrip: sr=%d len=%d, want %d/%d", sr, len(got), SampleRate, len(in))
	}
	for i := range in {
		if math.Abs(float64(got[i]-in[i])) > 1e-3 {
			t.Fatalf("sample %d differs: %v vs %v", i, in[i], got[i])
		}
	}
}

// maxDiff returns the largest absolute per-sample difference.
func maxDiff(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	d := 0.0
	for i := 0; i < n; i++ {
		if v := math.Abs(float64(a[i] - b[i])); v > d {
			d = v
		}
	}
	return d
}

// Chunked Process + Flush must equal a single Render sample-for-sample:
// the streaming path is the live-monitor path and any drift is audible.
func TestStreamingMatchesRender(t *testing.T) {
	in := sine(200, 0.5, SampleRate)
	for _, pitch := range []float64{-0.5, 0.5} {
		e := New()
		p := DefaultParams
		p.Pitch = pitch
		e.SetParams(p)
		full := e.Render(in)

		var streamed []float32
		const chunk = 2048
		for i := 0; i < len(in); i += chunk {
			end := i + chunk
			if end > len(in) {
				end = len(in)
			}
			streamed = append(streamed, e.Process(in[i:end])...)
		}
		streamed = append(streamed, e.Flush()...)

		if len(streamed) != len(full) {
			t.Fatalf("pitch %+v: streamed %d samples, render %d", pitch, len(streamed), len(full))
		}
		if d := maxDiff(streamed, full); d > 1e-5 {
			t.Fatalf("pitch %+v: streamed vs render max|diff| = %g", pitch, d)
		}
	}
}

// WSOLA must conserve length: output ≈ input/alpha regardless of the
// chunking it was fed (a stalled hop must not eat content).
func TestWSOLALengthConserved(t *testing.T) {
	in := sine(150, 1.0, SampleRate)
	x := make([]float64, len(in))
	for i, v := range in {
		x[i] = float64(v)
	}
	for _, alpha := range []float64{0.5, 1.0, 2.0} {
		w := NewWSOLA(alpha, 2048, 512, 256, SampleRate)
		var out []float64
		for i := 0; i < len(x); i += 2048 {
			end := i + 2048
			if end > len(x) {
				end = len(x)
			}
			out = append(out, w.Process(x[i:end])...)
		}
		out = append(out, w.Flush()...)
		want := float64(len(x)) / alpha
		if math.Abs(float64(len(out))-want) > 2*2048 {
			t.Errorf("alpha %v: got %d samples, want ~%.0f (±1 frame)", alpha, len(out), want)
		}
	}
}

// Malformed WAVs must return an error, never panic.
func TestDecodeWAVMalformed(t *testing.T) {
	good := EncodeWAV(sine(440, 0.05, SampleRate), SampleRate)
	cases := map[string][]byte{
		"empty":      {},
		"not-wave":   []byte("NOPE........WAVE"),
		"truncated":  good[:20],
		"huge-size":  wavWithHugeSize(),
		"short-fmt":  wavWithShortFmt(),
		"zero-bits":  wavWithBits(0),
		"weird-bits": wavWithBits(4),
	}
	for name, data := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panicked: %v", name, r)
				}
			}()
			if _, _, err := DecodeWAV(data); err == nil {
				t.Errorf("%s: expected error, got none", name)
			}
		}()
	}
}

// wavWithShortFmt builds a RIFF with a 4-byte fmt chunk and a data chunk.
func wavWithShortFmt() []byte {
	var b []byte
	b = append(b, "RIFF"...)
	b = append(b, 0, 0, 0, 0)
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = append(b, 4, 0, 0, 0, 1, 0, 0, 0) // size=4, body=4 bytes
	b = append(b, "data"...)
	b = append(b, 4, 0, 0, 0, 0, 0, 0, 0)
	return b
}

// wavWithBits builds a well-formed RIFF whose fmt chunk declares bps bits.
func wavWithBits(bps byte) []byte {
	data := EncodeWAV(sine(440, 0.01, SampleRate), SampleRate)
	data[34] = bps // bitsPerSample field inside fmt chunk
	return data
}

// wavWithHugeSize declares a ~4 GB fmt chunk followed by almost no data.
func wavWithHugeSize() []byte {
	data := EncodeWAV(sine(440, 0.01, SampleRate), SampleRate)[:24]
	data[16], data[17], data[18], data[19] = 0xFF, 0xFF, 0xFF, 0xF0
	return data
}
