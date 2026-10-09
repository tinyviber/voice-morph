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
	// a voiced comb, not a lone sine: with the orthogonal formant warp
	// (w/r) a single line is degenerate — its "envelope" rides on the
	// fundamental itself, so pulling the envelope back silences it
	in := vowel(120, 900, 1.0)
	for _, pitch := range []float64{-0.5, 0.5, 0.8} {
		e := New()
		p := DefaultParams
		p.Pitch = pitch
		e.SetParams(p)
		out := e.Render(in)
		want := 120 * math.Pow(2, pitch)
		got := estimateF0(out, SampleRate)
		if math.Abs(got-want)/want > 0.08 {
			t.Errorf("pitch %+v: f0 = %.1f Hz, want ~%.1f", pitch, got, want)
		}
		// Render is lip-synced: exact input length, zero tolerance
		if len(out) != len(in) {
			t.Errorf("pitch %+v: len(out) = %d, want exactly %d", pitch, len(out), len(in))
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
// Render additionally shifts output by the measured pipeline onset
// offset for lip-sync, so the comparison corrects for that shift:
// render[i] = pipeline[i+off], streamed[p] = pipeline[p].
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
		off := morphOffset(math.Pow(2, pitch))
		lo, hi := 0, len(in) // pipeline positions present in both
		if off > 0 {
			lo = off
		} else {
			hi = len(in) + off
		}
		if d := maxDiff(streamed[lo:hi], full[lo-off:hi-off]); d > 1e-5 {
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

// vowel synthesizes a harmonic-comb vowel: fundamental f0 with a
// spectral envelope peaked at fc — the closest thing to a sung vowel
// for envelope-peak tracking.
func vowel(f0, fc, dur float64) []float32 {
	n := int(dur * SampleRate)
	x := make([]float64, n)
	for k := 1; float64(k)*f0 < 20000; k++ {
		f := float64(k) * f0
		a := math.Exp(-math.Pow((f-fc)/350, 2))
		if a < 0.02 {
			continue
		}
		ph := float64(k) * 1.7
		for i := range x {
			x[i] += a * math.Sin(2*math.Pi*f*float64(i)/SampleRate+ph)
		}
	}
	mx := 0.0
	for _, v := range x {
		if math.Abs(v) > mx {
			mx = math.Abs(v)
		}
	}
	out := make([]float32, n)
	for i, v := range x {
		out[i] = float32(v / mx * 0.5)
	}
	return out
}

// bandEnvelopePeak finds the spectral-envelope peak like envelopePeak,
// but smooths the log spectrum over ~±234 Hz first so a harmonic comb's
// teeth (which sit inside the cepstral lifter's passband and jitter the
// peak) are averaged out.
func bandEnvelopePeak(x []float32, sr int) float64 {
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
	lm := make([]float64, n/2)
	for i := range lm {
		lm[i] = math.Log(math.Max(cmplxAbs(buf[i]), 1e-8))
	}
	const w = 40 // ±40 bins ≈ ±234 Hz, a few comb teeth at 120 Hz spacing
	sm := make([]float64, len(lm))
	for i := range lm {
		lo, hi := i-w, i+w+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(lm) {
			hi = len(lm)
		}
		s := 0.0
		for j := lo; j < hi; j++ {
			s += lm[j]
		}
		sm[i] = s / float64(hi-lo)
	}
	peak, pv := 0, -1e9
	for i := 40; i < len(sm); i++ {
		if sm[i] > pv {
			pv, peak = sm[i], i
		}
	}
	return float64(peak) * float64(sr) / float64(n)
}

// Pitch and Timbre are orthogonal: pitch alone must leave the spectral
// envelope in place (the formant stage is fed warp = w/r), timbre alone
// shifts it by 2^timbre.
func TestPitchTimbreOrthogonal(t *testing.T) {
	// pitch direction: a 900 Hz vowel resonance must stay put while f0
	// moves (unfixed, the resampler would drag it by r ≈ ±41%)
	in := vowel(120, 900, 1.0)
	base := bandEnvelopePeak(in, SampleRate)
	if base < 800 || base > 1000 {
		t.Fatalf("vowel envelope peak at %.0f Hz, want ~900", base)
	}
	for _, pitch := range []float64{-0.5, 0.5} {
		e := New()
		p := DefaultParams
		p.Pitch = pitch
		e.SetParams(p)
		out := e.Render(in)
		got := bandEnvelopePeak(out, SampleRate)
		if math.Abs(got/base-1) > 0.10 {
			t.Errorf("pitch %+v timbre 0: envelope peak %.0f→%.0f Hz (%.0f%%), want ±10%%",
				pitch, base, got, (got/base-1)*100)
		}
	}
	// timbre direction: a 1200 Hz resonance tracks the warp cleanly —
	// down-warps landing near ~700 Hz read ~10% hot on the 900 Hz vowel
	in2 := vowel(120, 1200, 1.0)
	base2 := bandEnvelopePeak(in2, SampleRate)
	for _, tim := range []float64{-0.4, 0.4} {
		e := New()
		p := DefaultParams
		p.Timbre = tim
		e.SetParams(p)
		out := e.Render(in2)
		got := bandEnvelopePeak(out, SampleRate)
		want := base2 * math.Pow(2, tim)
		if math.Abs(got/want-1) > 0.10 {
			t.Errorf("timbre %+v pitch 0: envelope peak %.0f→%.0f Hz, want ~%.0f",
				tim, base2, got, want)
		}
	}
}

// A morph-parameter change mid-stream must not drop output or click.
func TestSetParamsContinuity(t *testing.T) {
	in := sine(200, 1.0, SampleRate)
	e := New()
	p := DefaultParams
	p.Pitch = -0.3
	e.SetParams(p)

	var out []float32
	var blockLen []int
	const chunk = 2048
	nblocks := len(in) / chunk
	changeAt := 5
	for i := 0; i < nblocks; i++ {
		if i == changeAt {
			p.Pitch = 0.5
			e.SetParams(p)
		}
		o := e.Process(in[i*chunk : (i+1)*chunk])
		blockLen = append(blockLen, len(o))
		out = append(out, o...)
	}
	out = append(out, e.Flush()...)

	// (a) no gap larger than the steady pattern + ~one hop: pre-change
	// blocks emit ~2048 steadily once warmed; after the change no block
	// may emit less than 2048-512 while the stream is running
	for i := changeAt; i < nblocks; i++ {
		if blockLen[i] < 1024 {
			t.Fatalf("block %d emitted only %d samples after param change", i, blockLen[i])
		}
	}
	// (b) click detector: no sample-to-sample jump beyond 3× the
	// steady-state jump measured before the change
	pre := out[:4*chunk]
	steady := 0.0
	for i := 1; i < len(pre); i++ {
		if d := math.Abs(float64(pre[i] - pre[i-1])); d > steady {
			steady = d
		}
	}
	post := out[4*chunk:]
	mx := 0.0
	at := -1
	for i := 1; i < len(post); i++ {
		if d := math.Abs(float64(post[i] - post[i-1])); d > mx {
			mx, at = d, i
		}
	}
	if mx > 3*steady {
		t.Fatalf("click: max jump %.4f at sample %d > 3× steady %.4f", mx, at, steady)
	}
	// (c) total output within ~5% of input length (no lost content)
	if math.Abs(float64(len(out))/float64(len(in))-1) > 0.05 {
		t.Fatalf("streamed+flushed %d samples vs input %d (%.1f%% off)",
			len(out), len(in), (float64(len(out))/float64(len(in))-1)*100)
	}
}

// Toggling bypass mid-stream must blend dry↔wet instead of clicking.
func TestBypassContinuity(t *testing.T) {
	in := sine(200, 1.0, SampleRate)
	e := New()
	p := DefaultParams
	p.Pitch = 0.5
	e.SetParams(p)

	var out []float32
	const chunk = 2048
	for i := 0; i < len(in)/chunk; i++ {
		if i == 5 {
			p.Bypass = true
			e.SetParams(p)
		}
		if i == 12 {
			p.Bypass = false
			e.SetParams(p)
		}
		out = append(out, e.Process(in[i*chunk:(i+1)*chunk])...)
	}
	out = append(out, e.Flush()...)

	mx := 0.0
	for i := 1; i < len(out); i++ {
		if d := math.Abs(float64(out[i] - out[i-1])); d > mx {
			mx = d
		}
	}
	if mx > 0.5 {
		t.Fatalf("bypass toggle: max sample jump %.3f", mx)
	}
	if math.Abs(float64(len(out))/float64(len(in))-1) > 0.05 {
		t.Fatalf("bypass stream lost content: %d vs %d", len(out), len(in))
	}
}

// Render must be lip-synced for dubbing: exactly len(in) samples, and
// an onset must land within ~20 ms of its input position.
func TestRenderLipSync(t *testing.T) {
	ns := SampleRate * 3 / 10
	for _, pitch := range []float64{-1, -0.5, 0, 0.5, 1} {
		in := make([]float32, ns+SampleRate/2)
		for i := ns; i < len(in); i++ {
			in[i] = float32(0.5 * math.Sin(2*math.Pi*300*float64(i)/SampleRate))
		}
		e := New()
		p := DefaultParams
		p.Pitch = pitch
		e.SetParams(p)
		out := e.Render(in)
		if len(out) != len(in) {
			t.Fatalf("pitch %+v: len(out) = %d, want %d", pitch, len(out), len(in))
		}
		mx := 0.0
		for _, v := range out {
			if math.Abs(float64(v)) > mx {
				mx = math.Abs(float64(v))
			}
		}
		onset := -1
		for i, v := range out {
			if math.Abs(float64(v)) > 0.05*mx {
				onset = i
				break
			}
		}
		if off := onset - ns; off < -960 || off > 960 {
			t.Errorf("pitch %+v: onset offset %+d samples (%.1f ms), want |.| ≤ 20 ms",
				pitch, off, float64(off)/48)
		}
	}
}
