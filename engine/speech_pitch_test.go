package engine

import (
	"math"
	"slices"
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

// A target under half the source pitch (tgt > 2t) used to leave
// periodic uncovered gaps in the grain overlap-add — audible flutter of
// near-silence inside voiced runs. The fix: windows grow to 0.6·tgt
// (~20% neighbor overlap) with a floored edge whose tails read the
// mark's own pulse body wrapped and energy-normalized — coverage holds
// without smearing the waveform back to source periodicity, so the
// pulse-rate pitch cue still lands on the target.
func TestSpeechPitchDeepDownCoverage(t *testing.T) {
	// cases where the measured f0 must land on the target
	for _, c := range []struct{ src, dst float64 }{
		{300, 100}, {400, 100}, {200, 80}, {250, 110}, {350, 120},
	} {
		p := DefaultParams
		p.TargetF0 = c.dst
		out := runSpeechPitch(p, glottal(steady(c.src), 1, SampleRate), 2048)
		if got := estimateF0(f32(out), SampleRate); math.Abs(got-c.dst)/c.dst > 0.05 {
			t.Fatalf("%.0f→%.0f Hz: f0 %.1f Hz", c.src, c.dst, got)
		}
	}
	// every case, including the rough non-integer-ratio edges: no
	// 2.5 ms block may approach silence inside the voiced run. Before
	// the fix the uncovered spans read ≈0 (min/median ~0.03).
	for _, c := range []struct{ src, dst float64 }{
		{300, 100}, {400, 100}, {200, 80}, {250, 110}, {350, 120},
		{150, 60}, {180, 90}, {120, 80},
	} {
		p := DefaultParams
		p.TargetF0 = c.dst
		out := runSpeechPitch(p, glottal(steady(c.src), 1, SampleRate), 2048)
		lo, hi := len(out)/4, 3*len(out)/4
		const blk = 120
		var vals []float64
		for i := lo; i+blk <= hi; i += blk {
			vals = append(vals, rms(f32(out[i:i+blk])))
		}
		slices.Sort(vals)
		if med := vals[len(vals)/2]; vals[0] < 0.1*med {
			t.Fatalf("%.0f→%.0f Hz: min 2.5ms block rms %.4f vs median %.4f (coverage gap)",
				c.src, c.dst, vals[0], med)
		}
	}
	// vibrato (non-stationary) input keeps the same coverage and still
	// resolves the target pitch
	vib := func(t float64) float64 { return 300 * math.Exp2(0.4*math.Sin(2*math.Pi*3*t)/12) }
	p := DefaultParams
	p.TargetF0 = 100
	out := runSpeechPitch(p, glottal(vib, 1.2, SampleRate), 2048)
	if got := estimateF0(f32(out), SampleRate); math.Abs(got-100)/100 > 0.05 {
		t.Fatalf("vibrato 300→100 Hz: f0 %.1f Hz", got)
	}
}

// The soft knee keeps the normal intonation band (±4 st) exactly
// linear — PitchRange=1 preserves phrasing — and only compresses
// excursions past it, saturating toward ±8 st instead of clipping.
func TestLimitDevPreservesNormalRange(t *testing.T) {
	const st = math.Ln2 / 12
	for _, dev := range []float64{-4, -2.5, -1, 0, 2.5, 4} {
		if got := limitDev(dev * st); got != dev*st {
			t.Fatalf("limitDev(%.1f st) = %.3f st, want linear", dev, got/st)
		}
	}
	prev := limitDev(4 * st)
	for _, dev := range []float64{5, 7, 12, 40} {
		got := limitDev(dev * st)
		if got <= prev || got > 8*st {
			t.Fatalf("limitDev(%.0f st) = %.2f st: not monotone or over cap", dev, got/st)
		}
		prev = got
	}
	// extremes still compress, just softly: −7 st lands ~−6.5 st, not ±4
	if got := limitDev(7 * st); got >= 7*st || got <= 4*st {
		t.Fatalf("7 st mapped to %.2f st", got/st)
	}
	// C¹ at the knee: the slope continues smoothly past 4 st
	if d := limitDev(4.001*st) - limitDev(4*st); math.Abs(d-0.001*st) > 0.0005*st {
		t.Fatalf("knee discontinuity: step %.4f st", d/st)
	}
}

// A vibrato that just reaches the linear knee stays ~1:1 under
// PitchRange=1 — a global tanh used to compress its swing to ~0.75.
func TestSpeechPitchIntonationContour(t *testing.T) {
	vib := func(t float64) float64 { return 140 * math.Exp2(4.5*math.Sin(2*math.Pi*2*t)/12) }
	in := glottal(vib, 2, SampleRate)
	_, srcStd := meanStd(trackLogF0(in))
	p := DefaultParams
	p.TargetF0, p.PitchRange = 220, 1
	_, sd := meanStd(trackLogF0(runSpeechPitch(p, in, 2048)))
	if r := sd / srcStd; r < 0.9 || r > 1.12 {
		t.Fatalf("contour std ratio %.2f, want ~1.0 (PitchRange=1 preserves)", r)
	}
}

// The production voicing track exposes what the synthesizer decided per
// hop, so voiced misjudgments and F0 jumps can be located on a timeline.
func TestSpeechPitchTrace(t *testing.T) {
	e := New()
	p := DefaultParams
	p.Speech, p.TargetF0 = true, 200
	e.SetParams(p)
	in := f32(glottal(steady(120), 0.6, SampleRate))
	e.Render(in)
	tr := e.SpeechTrace()
	if len(tr) == 0 {
		t.Fatal("no production trace after a speech render")
	}
	var f0s []float64
	voiced := 0
	for i, fr := range tr {
		if i > 0 && fr.Pos <= tr[i-1].Pos {
			t.Fatalf("trace position %d not increasing", fr.Pos)
		}
		if !fr.Voiced {
			if fr.F0Out != 0 {
				t.Fatalf("unvoiced hop carries target f0 %v", fr.F0Out)
			}
			continue
		}
		voiced++
		f0s = append(f0s, fr.F0Out)
	}
	if voiced == 0 {
		t.Fatal("trace has no voiced hops")
	}
	slices.Sort(f0s)
	if med := f0s[len(f0s)/2]; math.Abs(med-200)/200 > 0.05 {
		t.Fatalf("median synthesized f0 %.1f, want ~200", med)
	}
	// legacy params produce no trace
	e2 := New()
	p.Speech = false
	e2.SetParams(p)
	e2.Render(in)
	if tr := e2.SpeechTrace(); tr != nil {
		t.Fatalf("legacy render left %d trace entries", len(tr))
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
