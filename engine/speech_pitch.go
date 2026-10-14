package engine

import (
	"math"
	"slices"
)

// SpeechPitch is a fixed-grid 1:1 TD-PSOLA pitch stage for speech. It
// analyzes centered frames on a regular hop grid, overlap-adds pitch
// grains into a wet buffer on the SAME sample clock as its input, and
// crossfades dry→wet by voicing. Unlike the resample+WSOLA path it never
// scales the timeline: output position p carries input content p, so
// unvoiced content (fricatives, breath) passes through untouched instead
// of being pitch-shifted like periodic speech.
//
// Determinism is a hard contract: every state update happens on an input
// position or on the fixed hop grid, never at a Process boundary, and a
// frame reads no input past its own lookahead frontier, so any input
// chunking produces the identical stream.
type SpeechPitch struct {
	sr   int
	hop  int // analysis grid (512 @48k)
	win  int // lookahead/buffer span (3072 @48k)
	ana  int // tracker frame length (1536 @48k), ≤ win
	minT int // shortest source period (500 Hz)
	maxT int // longest source period (60 Hz)
	p    Params

	tr *PitchTracker
	fr []float64

	in    []float64
	base  int // absolute index of in[0]
	avail int // absolute input frontier
	lim   int // reads at or past lim return 0: the current frame's lookahead
	nextC int // next analysis-frame center on the hop grid
	emit  int // next output position
	done  int // positions below done receive no further grain writes

	wet      []float64 // grain overlap-add; wet[i] is position wetB+i
	wetB     int
	mark     []int     // source pitch marks of the current voiced run
	seg, ref []float64 // nextMark scratch

	voice []float64 // per-hop voicing (0/1); voice[i] is hop hbase+i
	hbase int

	// Synthesis cursor: the next grain center plus the last voiced hop's
	// source period, grain half-length, target period and grain gain. A
	// voiced run fades out over the following hop with these.
	next    float64
	running bool
	t, gt   int
	tgt     float64
	g       float64
	gf      float64 // grain-window floor in the deep-down gap regime
	gtMax   int     // widest grain half-width used so far (bounds done/drop)

	hist []float64 // recent voiced log-F0, oldest first
	srt  []float64 // median scratch
	mu   float64   // source log-F0 center: running median of hist

	ew, ed float64 // decaying wet/dry energy over fully voiced spans
	gain   float64 // wet level correction at done

	trace []SpeechFrame // per-hop production voicing track

	flushed bool
}

// SpeechFrame is one hop of the production voicing track: what the
// tracker decided and which target pitch the synthesizer actually used.
// The offline evaluator reports it per rendered clip so voiced/unvoiced
// misjudgments, F0 jumps and artifact positions are locatable — the
// summary voicedRatio alone cannot distinguish them.
type SpeechFrame struct {
	Pos        int     `json:"pos"`        // input position of the analysis center
	Voiced     bool    `json:"voiced"`     // the decision synthesis ran on
	F0In       float64 `json:"f0In,omitempty"`
	F0Out      float64 `json:"f0Out,omitempty"` // synthesized target F0 (voiced only)
	Confidence float64 `json:"confidence"`
}

const (
	spHop = 512
	spWin = 3072
	// spAna is the tracker frame length. It is shorter than spWin on
	// purpose: 64 ms of context blurs a fast F0 glide (aew's low-pitched
	// sentence-final falls) into missed or wrong estimates, while the
	// lookahead spWin affords stays available for grain synthesis.
	spAna   = 1536
	spMinF0 = 60.0
	spMaxF0 = 500.0
	// spMeanHops bounds the source log-F0 center's memory (~2.7 s of
	// voiced speech) so it follows the speaker, not the intonation
	// PitchRange scales around it.
	spMeanHops = 256
	// spLevelDecay is the per-hop memory of the wet/dry level match
	// (~50 ms): fast enough for a syllable, slow enough to average
	// pulse-position noise.
	spLevelDecay = 0.8
	// spDevKnee bounds the intonation band mapped 1:1: PitchRange=1
	// preserves the speaker's own phrasing inside ±4 st of the center.
	spDevKnee = 4 * math.Ln2 / 12
	// spDevCap asymptotically bounds the output swing so extreme falls
	// and exaggerated ranges compress instead of digging a pitch hole
	// at the 40 Hz output clamp.
	spDevCap = 8 * math.Ln2 / 12
)

// limitDev applies the soft knee to a PitchRange-scaled log-F0
// deviation: exactly linear inside ±spDevKnee, then a C¹-continuous
// tanh segment saturating toward ±spDevCap. (The earlier unconditional
// tanh silently compressed every contour — pitchRange=1 turned ±4 st
// into ±3.05 st, i.e. a built-in dynamics compressor on intonation.)
func limitDev(dev float64) float64 {
	if d := math.Abs(dev); d > spDevKnee {
		return math.Copysign(spDevKnee+(spDevCap-spDevKnee)*math.Tanh((d-spDevKnee)/(spDevCap-spDevKnee)), dev)
	}
	return dev
}

func NewSpeechPitch(p Params, sr int) *SpeechPitch {
	if sr <= 0 {
		sr = SampleRate
	}
	s := &SpeechPitch{
		sr:   sr,
		hop:  spHop * sr / SampleRate,
		win:  spWin * sr / SampleRate,
		ana:  spAna * sr / SampleRate,
		minT: int(math.Ceil(float64(sr) / spMaxF0)),
		maxT: int(math.Ceil(float64(sr) / spMinF0)),
		p:    p.normalized(),
		tr:   NewPitchTracker(sr),
		next: math.Inf(-1),
		gain: 1,
	}
	s.fr = make([]float64, s.ana)
	s.done = -2*s.maxT - s.hop
	return s
}

// pitchNeutral reports whether the speech pitch stage would be identity.
func pitchNeutral(p Params) bool {
	return p.Pitch == 0 && p.TargetF0 == 0 && p.PitchRange == 1
}

// Process appends input and returns every output position that is final.
// The fixed lookahead is win/2 + 2·maxT (~65 ms at 48 kHz); Flush emits
// the deferred tail.
func (s *SpeechPitch) Process(x []float64) []float64 {
	if s.flushed || len(x) == 0 {
		return nil
	}
	s.in = append(s.in, x...)
	s.avail += len(x)
	for s.nextC+s.win/2 <= s.avail {
		s.frame(s.nextC)
		s.nextC += s.hop
	}
	return s.drain(s.done)
}

// Flush analyzes the remaining frames over silence and emits the rest, so
// total output length equals total input length.
func (s *SpeechPitch) Flush() []float64 {
	if s.flushed {
		return nil
	}
	s.flushed = true
	// one hop past the last frame that sees input lets a voiced run end
	// with its fade-out; every emitted position must also be leveled
	for s.nextC < s.avail+s.win/2+s.hop || s.done < s.avail {
		s.frame(s.nextC)
		s.nextC += s.hop
	}
	return s.drain(s.avail)
}

// at reads input at absolute position i: 0 outside the retained buffer
// and at or past the current frame's lookahead frontier.
func (s *SpeechPitch) at(i int) float64 {
	if i >= s.lim {
		return 0
	}
	if j := i - s.base; j >= 0 && j < len(s.in) {
		return s.in[j]
	}
	return 0
}

// frame analyzes the window centered at c, records the hop's voicing and
// overlap-adds every grain centered in this hop. It then levels and marks
// final the positions no later hop can write.
func (s *SpeechPitch) frame(c int) {
	s.lim = c + s.win/2
	for i := range s.fr {
		s.fr[i] = s.at(c - s.ana/2 + i)
	}
	hint := 0.0
	if s.running {
		hint = float64(s.sr) / float64(s.t)
	}
	e := s.tr.AnalyzeNear(s.fr, hint)
	v := 0.0
	fout := 0.0
	switch {
	case e.Voiced:
		v = 1
		fout = s.voiced(c, e.F0)
	case s.running:
		// fade the run out across this hop by repeating its last period
		// at the last target pitch
		s.synthesize(c + s.t)
		s.running = false
	}
	s.trace = append(s.trace, SpeechFrame{Pos: c, Voiced: e.Voiced, F0In: e.F0, F0Out: fout, Confidence: e.Confidence})
	s.voice = append(s.voice, v)
	// the next hop writes no earlier than its fade-in start minus a
	// grain half-width: (c+hop) − hop − t − gt ≥ c − maxT − gmax, where
	// gmax = max(gtMax, maxT) covers deep target periods beyond 60 Hz
	prev := s.done
	s.done = c - s.maxT - max(s.gtMax, s.maxT)
	s.level(prev, s.done)
	s.drop(c)
}

// voiced sets this hop's grain parameters and synthesizes its grains:
// logF0_out = center + Pitch·ln2 + PitchRange·(logF0 − mu), where mu is
// the running median of the source and the center is mu unless TargetF0
// pins it. A median keeps creaky onsets and octave slips from dragging
// the center the way a mean would. The center is causal: within a
// declining sentence it lags above the current F0, so TargetF0 is the
// long-run voiced median, not a per-utterance one. Returns the
// target F0 the hop was synthesized at, for the voicing trace.
func (s *SpeechPitch) voiced(c int, f0 float64) float64 {
	l := math.Log(f0)
	// Estimates pinned at the tracker floor are vocal fry or a missed
	// fundamental; either way they say nothing about the speaker's center.
	if f0 > spMinF0*1.05 {
		if len(s.hist) == spMeanHops {
			copy(s.hist, s.hist[1:])
			s.hist = s.hist[:spMeanHops-1]
		}
		s.hist = append(s.hist, l)
	}
	s.mu = l
	if n := len(s.hist); n > 0 {
		s.srt = append(s.srt[:0], s.hist...)
		slices.Sort(s.srt)
		s.mu = (s.srt[(n-1)/2] + s.srt[n/2]) / 2
	}
	center := s.mu
	if s.p.TargetF0 > 0 {
		center = math.Log(s.p.TargetF0)
	}
	// Cap the OUTPUT deviation rather than the input's: PitchRange>1
	// keeps intonation lively inside roughly a musical third around the
	// center, but a sentence-final fall can reach −7 st under mu; amplified
	// and pinned under a 280 Hz target that lands ~203 Hz — a pitch hole
	// next to the surrounding 270 that reads as a crack. limitDev keeps
	// the ±4 st band linear and only compresses excursions past it.
	dev := limitDev(s.p.PitchRange * (l - s.mu))
	out := clamp(math.Exp(center+s.p.Pitch*math.Ln2+dev), 40, 900)
	t := max(s.minT, min(s.maxT, int(math.Round(float64(s.sr)/f0))))
	if !s.running {
		// A new run fades in over the previous hop: start grains a
		// half-width earlier so the overlap-add is complete there. A
		// fade-out that already reached this far continues instead.
		s.mark = s.mark[:0]
		s.next = math.Max(s.next, float64(c-s.hop-t))
		s.running = true
	}
	s.t, s.tgt = t, float64(s.sr)/out
	// Coverage: grain centers land tgt apart, so a 2t window leaves
	// periodic holes whenever tgt > 2t (the mute flutter the review
	// flagged). The half-width grows to 0.6·tgt instead — a ~20%
	// neighbor overlap plus the floored edge below — without stretching
	// to the full two target periods, which would smear the waveform
	// back toward the source's own periodicity and lose the shift
	// entirely (a 300→100 Hz move would measure ~300 Hz again).
	s.gt = min(t, int(math.Round(s.tgt)))
	if g2 := int(math.Ceil(s.tgt * 0.6)); g2 > s.gt {
		s.gt = g2
	}
	if s.gt > s.gtMax {
		s.gtMax = s.gt
	}
	// In the gap regime the seams between grains sit on floored edges:
	// the floor holds at 0.4 of real voiced content (see the tail read
	// in grain), leaving ~7 dB of pulse modulation as the low-pitch cue
	// while nothing approaches silence. Below the regime the floor is
	// exactly 0 so the COLA overlap for raising and mild lowering is
	// untouched.
	if r := s.tgt / float64(t); r >= 1.8 {
		s.gf = 0.4
	} else {
		s.gf = 0
	}
	// prior for packing more grains per second; level() corrects the rest
	s.g = math.Min(1, math.Sqrt(s.tgt/float64(t)))
	s.extendMarks(c, t)
	s.synthesize(c + s.hop/2)
	return out
}

// extendMarks places one source mark per period up to the hop's end. The
// first mark of a run sits on the largest |x| near the hop start; later
// marks follow by waveform similarity, so every mark has the same glottal
// phase.
func (s *SpeechPitch) extendMarks(c, t int) {
	end := c + s.hop/2
	if len(s.mark) == 0 {
		s.mark = append(s.mark, s.peak(c-s.hop/2, t))
	}
	for last := s.mark[len(s.mark)-1]; last+t <= end; last = s.mark[len(s.mark)-1] {
		s.mark = append(s.mark, s.nextMark(last, t))
	}
}

func (s *SpeechPitch) peak(c, t int) int {
	best, bestV := c, math.Abs(s.at(c))
	for p := c - t/4; p <= c+t/4; p++ {
		if v := math.Abs(s.at(p)); v > bestV {
			best, bestV = p, v
		}
	}
	return best
}

// nextMark returns the position within last+t ± t/4 whose one-period
// neighborhood best correlates (normalized) with last's. Picking |x|
// peaks instead lets marks hop between formant-ringing lobes, and
// inconsistent mark phase turns into output jitter. A coarse search on
// every 4th sample is refined at full resolution.
func (s *SpeechPitch) nextMark(last, t int) int {
	r, h := t/4, t/2
	lo := last + t - r - h
	s.seg = resizeFloat(s.seg, 2*(r+h)+1)
	for i := range s.seg {
		s.seg[i] = s.at(lo + i)
	}
	s.ref = resizeFloat(s.ref, 2*h)
	for i := range s.ref {
		s.ref[i] = s.at(last - h + i)
	}
	score := func(q, step int) float64 {
		var c, e float64
		b := s.seg[q-h-lo:]
		for j := 0; j < 2*h; j += step {
			c += s.ref[j] * b[j]
			e += b[j] * b[j]
		}
		if e <= 0 {
			return 0
		}
		return c / math.Sqrt(e)
	}
	// ties (silence) keep the nominal period
	best, bestC := last+t, score(last+t, 4)
	for q := last + t - r; q <= last+t+r; q += 2 {
		if c := score(q, 4); c > bestC {
			best, bestC = q, c
		}
	}
	q0 := best
	bestC = score(q0, 1)
	for q := max(q0-2, last+t-r); q <= min(q0+2, last+t+r); q++ {
		if c := score(q, 1); c > bestC {
			best, bestC = q, c
		}
	}
	return best
}

// synthesize overlap-adds grains centered every target period from the
// cursor up to until. Each grain is a Hann window around the source mark
// nearest its center: reusing or skipping marks as the target period
// departs from the source period changes pitch without moving content.
func (s *SpeechPitch) synthesize(until int) {
	for ; s.next < float64(until); s.next += s.tgt {
		c := int(math.Round(s.next))
		s.grain(c, s.nearestMark(c))
	}
}

func (s *SpeechPitch) nearestMark(p int) int {
	best := s.mark[0]
	for _, m := range s.mark[1:] {
		if abs(p-m) < abs(p-best) {
			best = m
		}
	}
	return best
}

func (s *SpeechPitch) grain(to, mark int) {
	t := s.gt
	l := to - t
	if len(s.wet) == 0 {
		s.wetB = l
	}
	if l < s.wetB {
		pad := make([]float64, s.wetB-l, s.wetB-l+len(s.wet))
		s.wet = append(pad, s.wet...)
		s.wetB = l
	}
	if n := l + 2*t - s.wetB; n > len(s.wet) {
		s.wet = append(s.wet, make([]float64, n-len(s.wet))...)
	}
	w := s.wet[l-s.wetB:]
	// In the gap regime the window tails tile the inter-pulse span; a
	// linear read there lands on the source's own troughs whenever
	// tgt/2 is not a multiple of t, recreating the dips one level up.
	// The tails instead read the mark's own ±t/2 neighborhood wrapped
	// — the pulse body itself — and are normalized to a fraction of
	// the grain's body energy, so the floor holds real voiced energy
	// instead of chasing the source's silence.
	k := 1.0
	if s.gf > 0 {
		var eb, et float64
		for j := range 2 * t {
			r := mark - t + j
			if d := j - t; d >= -s.t/2 && d < s.t/2 {
				v := s.at(r)
				eb += v * v
			} else {
				v := s.at(mark + wrap(r-mark+s.t/2, s.t) - s.t/2)
				et += v * v
			}
		}
		if et > 0 && eb > 0 {
			k = math.Min(3, math.Sqrt(eb/et))
		}
	}
	for j := range 2 * t {
		h := s.gf + (1-s.gf)*(0.5-0.5*math.Cos(math.Pi*float64(j)/float64(t)))
		r := mark - t + j
		if s.gf > 0 {
			if d := r - mark; d < -s.t/2 || d >= s.t/2 {
				// tail: the mark's ±t/2 body, wrapped, rescaled to
				// the grain's own body energy
				r = mark + wrap(d+s.t/2, s.t) - s.t/2
				w[j] += s.g * h * k * s.at(r)
				continue
			}
		}
		w[j] += s.g * h * s.at(r)
	}
}

// wrap returns v reduced into [0, m): positive mod for m>0.
func wrap(v, m int) int {
	q := v % m
	if q < 0 {
		q += m
	}
	return q
}

// level matches wet power to the dry input's over fully voiced spans.
// Overlap-adding grains at a new period changes power by a signal-
// dependent amount (pulse trains gain with rate, smooth content loses
// Hann² overlap), so it is measured: decaying wet/dry energy sums over
// the newly final span [lo, hi) set a gain, ramped across the span.
func (s *SpeechPitch) level(lo, hi int) {
	if hi <= lo {
		return
	}
	g := s.gain
	full := true
	for h := lo / s.hop; h <= (hi-1)/s.hop+1; h++ {
		full = full && s.voiceAt(h) == 1
	}
	if full {
		var ew, ed float64
		for p := lo; p < hi; p++ {
			w, d := s.wetAt(p), s.at(p)
			ew += w * w
			ed += d * d
		}
		s.ew = spLevelDecay*s.ew + ew
		s.ed = spLevelDecay*s.ed + ed
		if s.ew > 1e-12 {
			g = clamp(math.Sqrt(s.ed/s.ew), 0.5, 2)
		}
	}
	for p := lo; p < hi; p++ {
		if j := p - s.wetB; j >= 0 && j < len(s.wet) {
			s.wet[j] *= s.gain + (g-s.gain)*float64(p-lo)/float64(hi-lo)
		}
	}
	s.gain = g
}

func (s *SpeechPitch) wetAt(p int) float64 {
	if j := p - s.wetB; j >= 0 && j < len(s.wet) {
		return s.wet[j]
	}
	return 0
}

// drain emits positions [emit, limit): dry crossfaded to wet by the voicing
// track, interpolated linearly between hop centers so voiced/unvoiced
// transitions fade over one hop instead of clicking.
func (s *SpeechPitch) drain(limit int) []float64 {
	if limit <= s.emit {
		return nil
	}
	out := make([]float64, limit-s.emit)
	for i := range out {
		p := s.emit + i
		h := p / s.hop
		f := float64(p-h*s.hop) / float64(s.hop)
		v := s.voiceAt(h)*(1-f) + s.voiceAt(h+1)*f
		dry := s.at(p)
		out[i] = dry + (s.wetAt(p)-dry)*v
	}
	s.emit = limit
	return out
}

func (s *SpeechPitch) voiceAt(h int) float64 {
	if i := h - s.hbase; i >= 0 && i < len(s.voice) {
		return s.voice[i]
	}
	return 0
}

// Trace returns the production voicing track: one entry per analysis
// hop in stream positions.
func (s *SpeechPitch) Trace() []SpeechFrame {
	return append([]SpeechFrame(nil), s.trace...)
}

// drop releases state no future read can reach. After frame c every
// later grain centers at ≥ c−maxT and reads a mark within ~2 periods
// of its center plus a grain half-width, so input below c−3·gmax−hop
// is dead, as is a mark whose successor already lies below that bound
// plus gmax. Wet and voicing are read only from emit on. Capping by
// emit keeps the bound valid however the input was chunked.
func (s *SpeechPitch) drop(c int) {
	// gmax is the period bound the accounting below shares: marks stay
	// within ~2 periods of a grain center and grains are gmax wide, so
	// nothing below the bounds is ever read again. Widening it only
	// retains more state, which is safe at any parameter set.
	gmax := max(s.gtMax, s.maxT)
	keep := min(s.emit, c-3*gmax-s.hop)
	if d := min(keep-s.base, len(s.in)); d > 0 {
		copy(s.in, s.in[d:])
		s.in = s.in[:len(s.in)-d]
		s.base += d
	}
	if d := min(s.emit-s.wetB, len(s.wet)); d > 0 {
		copy(s.wet, s.wet[d:])
		s.wet = s.wet[:len(s.wet)-d]
		s.wetB += d
	}
	if d := min(s.emit/s.hop-1-s.hbase, len(s.voice)); d > 0 {
		copy(s.voice, s.voice[d:])
		s.voice = s.voice[:len(s.voice)-d]
		s.hbase += d
	}
	for len(s.mark) > 1 && s.mark[1] <= keep+gmax {
		s.mark = s.mark[1:]
	}
}
