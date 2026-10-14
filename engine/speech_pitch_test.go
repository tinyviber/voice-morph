package engine

import (
	"math"
	"testing"
)

// glottal builds a voiced test signal: a pulse train whose period follows
// f0(t), each pulse a decaying burst. It is closer to speech excitation
// than a sine, so pitch marks have real peaks to lock onto.
func glottal(f0 func(t float64) float64, dur float64, sr int) []float64 {
	n := int(dur * float64(sr))
	x := make([]float64, n)
	phase, start := 1.0, 0
	t := 1
	for i := range x {
		if phase >= 1 {
			phase -= 1
			start = i
			t = int(float64(sr) / f0(float64(i)/float64(sr)))
		}
		x[i] = 0.6 * math.Exp(-4*float64(i-start)/float64(t))
		phase += f0(float64(i)/float64(sr)) / float64(sr)
	}
	return x
}

func steady(f0 float64) func(float64) float64 { return func(float64) float64 { return f0 } }

func f32(x []float64) []float32 {
	out := make([]float32, len(x))
	for i, v := range x {
		out[i] = float32(v)
	}
	return out
}

func runSpeechPitch(p Params, x []float64, chunk int) []float64 {
	s := NewSpeechPitch(p, SampleRate)
	var out []float64
	for i := 0; i < len(x); i += chunk {
		out = append(out, s.Process(x[i:min(i+chunk, len(x))])...)
	}
	return append(out, s.Flush()...)
}

// trackLogF0 returns log-F0 of every voiced 3072-sample frame of x at
// a 512-sample hop, skipping the outer quarter at both ends.
func trackLogF0(x []float64) []float64 {
	tr := NewPitchTracker(SampleRate)
	var out []float64
	for c := len(x) / 4; c+3072 <= 3*len(x)/4; c += 512 {
		if e := tr.Analyze(x[c : c+3072]); e.Voiced {
			out = append(out, math.Log(e.F0))
		}
	}
	return out
}

func meanStd(x []float64) (float64, float64) {
	var m, v float64
	for _, y := range x {
		m += y
	}
	m /= float64(len(x))
	for _, y := range x {
		v += (y - m) * (y - m)
	}
	return m, math.Sqrt(v / float64(len(x)))
}

func TestSpeechPitchLengthConserved(t *testing.T) {
	in := glottal(steady(120), 0.5, SampleRate)
	for _, pitch := range []float64{-0.5, 0.8} {
		for _, n := range []int{len(in), 1000, 1} {
			p := DefaultParams
			p.Pitch = pitch
			if out := runSpeechPitch(p, in[:n], 512); len(out) != n {
				t.Fatalf("pitch %+v len %d: output %d samples", pitch, n, len(out))
			}
		}
	}
}

func TestSpeechPitchChunkInvariant(t *testing.T) {
	// glide + noise floor so voicing, marks and the F0 center all move
	in := glottal(func(t float64) float64 { return 110 + 90*t }, 0.8, SampleRate)
	for i := range in[:len(in)/3] {
		in[i] += 0.02 * math.Sin(float64(i)*float64(i)*1e-3)
	}
	p := DefaultParams
	p.Pitch, p.PitchRange = 0.6, 1.3
	whole := runSpeechPitch(p, in, len(in))
	for _, chunk := range []int{1, 97, 512, 2048} {
		out := runSpeechPitch(p, in, chunk)
		if len(out) != len(whole) {
			t.Fatalf("chunk %d: %d samples, want %d", chunk, len(out), len(whole))
		}
		for i := range out {
			if out[i] != whole[i] {
				t.Fatalf("chunk %d: sample %d = %v, want %v", chunk, i, out[i], whole[i])
			}
		}
	}
}

func TestSpeechPitchShiftsF0(t *testing.T) {
	for _, c := range []struct{ src, pitch float64 }{
		{120, 1}, {120, 0.5}, {200, -0.5}, {100, 0.8},
	} {
		p := DefaultParams
		p.Pitch = c.pitch
		out := runSpeechPitch(p, glottal(steady(c.src), 1, SampleRate), 2048)
		want := c.src * math.Exp2(c.pitch)
		got := estimateF0(f32(out), SampleRate)
		if math.Abs(got-want)/want > 0.03 {
			t.Fatalf("src %.0f Hz pitch %+v: f0 %.1f Hz, want %.1f", c.src, c.pitch, got, want)
		}
		if r := rms(f32(out[len(out)/4 : 3*len(out)/4])); r < 0.05 {
			t.Fatalf("src %.0f Hz pitch %+v: level %.3f", c.src, c.pitch, r)
		}
	}
}

func TestSpeechPitchTargetF0(t *testing.T) {
	// TargetF0 sets the voiced center independently of the input speaker.
	for _, src := range []float64{100, 160} {
		p := DefaultParams
		p.TargetF0 = 260
		out := runSpeechPitch(p, glottal(steady(src), 1, SampleRate), 2048)
		if got := estimateF0(f32(out), SampleRate); math.Abs(got-260)/260 > 0.03 {
			t.Fatalf("src %.0f Hz: f0 %.1f Hz, want ~260 Hz", src, got)
		}
	}
}

func TestSpeechPitchRange(t *testing.T) {
	// ±0.15 octave vibrato around 140 Hz: PitchRange scales the log-F0
	// deviation around the (target) center, 0 flattens it.
	vib := func(t float64) float64 { return 140 * math.Exp2(0.15*math.Sin(2*math.Pi*2*t)) }
	in := glottal(vib, 2, SampleRate)
	_, srcStd := meanStd(trackLogF0(in))
	for _, c := range []struct{ rng, lo, hi float64 }{
		{0, 0, 0.25}, {1, 0.75, 1.25}, {1.5, 1.2, 1.8},
	} {
		p := DefaultParams
		p.TargetF0, p.PitchRange = 220, c.rng
		m, sd := meanStd(trackLogF0(runSpeechPitch(p, in, 2048)))
		if ratio := sd / srcStd; ratio < c.lo || ratio > c.hi {
			t.Fatalf("range %.1f: std ratio %.2f, want [%.2f, %.2f]", c.rng, ratio, c.lo, c.hi)
		}
		if f := math.Exp(m); math.Abs(f-220)/220 > 0.05 {
			t.Fatalf("range %.1f: mean f0 %.1f Hz, want ~220", c.rng, f)
		}
	}
}

func TestSpeechPitchKeepsUnvoiced(t *testing.T) {
	// noise is unvoiced: it must pass through untouched on its own clock
	in := make([]float64, SampleRate/2)
	seed := uint32(1)
	for i := range in {
		seed = seed*1664525 + 1013904223
		in[i] = (float64(seed)/(1<<32) - 0.5) * 0.4
	}
	p := DefaultParams
	p.Pitch = 1
	out := runSpeechPitch(p, in, 2048)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("unvoiced sample %d = %v, want dry %v", i, out[i], in[i])
		}
	}
}

func TestSpeechPitchVoicedOnsetOnTime(t *testing.T) {
	// silence → voice: the onset must stay where it was (1:1 timeline)
	in := make([]float64, SampleRate)
	copy(in[SampleRate/2:], glottal(steady(150), 0.5, SampleRate))
	p := DefaultParams
	p.Pitch = 0.7
	out := runSpeechPitch(p, in, 2048)
	env := func(x []float64, i int) float64 { return rms(f32(x[i : i+480])) }
	for i := 0; i+480 <= len(in); i += 480 {
		if (env(in, i) > 0.05) != (env(out, i) > 0.02) {
			t.Fatalf("10 ms block @%d: in rms %.3f, out rms %.3f", i, env(in, i), env(out, i))
		}
	}
}

func TestSpeechPitchNeutral(t *testing.T) {
	p := DefaultParams
	p.Speech = true
	if !NewSpeechChain(p, SampleRate).Transparent() {
		t.Fatal("neutral speech params should give a transparent chain")
	}
	p.PitchRange = 0 // flattens intonation: still a transform
	if NewSpeechChain(p, SampleRate).Transparent() {
		t.Fatal("PitchRange 0 should not be transparent")
	}
}

// Switching between legacy and speech presets mid-stream mixes chains
// with different Offset priors; the crossfade must neither stall nor
// replay content (same contract as TestRepeatedSwitchStability).
func TestSpeechLegacySwitchStability(t *testing.T) {
	in := burstTrack(700, 0.5, 240, 2160, SampleRate)
	byID := map[string]Params{}
	for _, pr := range Presets {
		byID[pr.ID] = pr.Params
	}
	order := []string{"natural-woman", "woman", "natural-child", "child", "normal"}
	e := New()
	var out []float32
	var blockLen []int
	const chunk = 2048
	for i := 0; i < len(in); i += chunk {
		if b := i / chunk; b%4 == 0 {
			e.SetParams(byID[order[(b/4)%len(order)]])
		}
		o := e.Process(in[i:min(i+chunk, len(in))])
		blockLen = append(blockLen, len(o))
		out = append(out, o...)
	}
	out = append(out, e.Flush()...)
	short := 0
	for i := 6; i < len(blockLen); i++ {
		if short = short + 1; blockLen[i] >= 512 {
			short = 0
		}
		if short > stallBlocks {
			t.Fatalf("block %d: %d stalled blocks in a row", i, short)
		}
	}
	assertOnsets1to1(t, in, out, 960, 0.6)
}

// The speech path must satisfy the same streaming≡Render contract as the
// legacy path (see TestStreamingMatchesRender).
func TestSpeechStreamingMatchesRender(t *testing.T) {
	in := f32(glottal(func(t float64) float64 { return 120 + 60*t }, 0.7, SampleRate))
	for _, id := range []string{"natural-woman", "natural-child"} {
		var p Params
		for _, pr := range Presets {
			if pr.ID == id {
				p = pr.Params
			}
		}
		e := New()
		e.SetParams(p)
		full := e.Render(in)

		pad := renderPad(0)
		padded := append(make([]float32, pad), in...)
		var streamed []float32
		for i := 0; i < len(padded); i += 2048 {
			streamed = append(streamed, e.Process(padded[i:min(i+2048, len(padded))])...)
		}
		streamed = append(streamed, e.Flush()...)
		xf, sf := make([]float64, len(padded)), make([]float64, len(streamed))
		for i, v := range padded {
			xf[i] = float64(v)
		}
		for i, v := range streamed {
			sf[i] = float64(v)
		}
		off := alignLag(xf, sf, 0)
		if abs(off) > 96 {
			t.Fatalf("%s: onset offset %d samples, want ~0 (1:1 timeline)", id, off)
		}
		for i := range in {
			if j := i + pad + off; j >= 0 && j < len(streamed) && math.Abs(float64(full[i]-streamed[j])) > 1e-5 {
				t.Fatalf("%s: sample %d render %v, streamed %v", id, i, full[i], streamed[j])
			}
		}
	}
}

func BenchmarkSpeechChainProcess(b *testing.B) {
	b.ReportAllocs()
	x, chunk := benchClip()
	p := DefaultParams
	for _, pr := range Presets {
		if pr.ID == "natural-woman" {
			p = pr.Params
		}
	}
	c := NewSpeechChain(p, SampleRate)
	nchunks := len(x) / chunk
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := (i % nchunks) * chunk
		c.Process(x[off : off+chunk])
	}
}
