package dubbing

import (
	"fmt"
	"sort"
	"strings"
)

// ReconcileOptions tunes the script↔audio matching. All scoring is
// deterministic (edit distance over normalized characters + a dynamic-
// programming block aligner); no LLM or semantic model is required.
type ReconcileOptions struct {
	// MatchFloor: block matches below this similarity are impossible —
	// the lines fall back to "no audio" and the segments to unmatched.
	MatchFloor float64
	// DP costs: skipping a line costs SkipLineCost, skipping a segment
	// costs SkipSegCost; a match costs (1-sim) + MergePenalty per extra
	// element in the block.
	SkipLineCost float64
	SkipSegCost  float64
	MergePenalty float64
	// ReviewSimilarity: bound lines below this get review_required.
	ReviewSimilarity float64
	// ExactSimilarity: at/above this the match_status is "exact".
	ExactSimilarity float64
	// TakeSimilarity: a standalone segment scoring this against a bound
	// line is treated as a candidate take.
	TakeSimilarity float64
	// PeelSimilarity: inside a matched block, a source segment whose
	// aligned coverage is below this share of its own text is reclassified
	// as unmatched instead of taking part in the binding (false starts,
	// coughs). Set ≤0 to disable peeling.
	PeelSimilarity float64
	// LineCoverageFloor: a line whose aligned coverage is below this share
	// of its own normalized length is treated as unread (no_audio) — a
	// handful of coincidental character matches must not bind a skipped
	// line to someone else's audio.
	LineCoverageFloor float64
	// LineSimFloor: a line whose aligned span similarity (exact matches
	// only) is below this is peeled — the audio it would steal almost
	// certainly belongs elsewhere (e.g. a retake of the previous line).
	// Between this and ReviewSimilarity the line stays bound but flagged.
	LineSimFloor float64
	// TakeAmbiguity: when the two best takes are closer than this, all
	// takes are flagged ambiguous_takes for review.
	TakeAmbiguity float64
	// ASRConfidenceFloor flags low_asr_confidence when the backend
	// reports confidence below it.
	ASRConfidenceFloor float64
	// MaxLinesPerBlock / MaxSegsPerBlock bound the DP merge widths.
	MaxLinesPerBlock int
	MaxSegsPerBlock  int
	// WordSnapMs: estimated interior boundaries are snapped to a word
	// timestamp edge when one lies this close.
	WordSnapMs int
}

// DefaultReconcileOptions returns the tuned defaults.
func DefaultReconcileOptions() ReconcileOptions {
	return ReconcileOptions{
		MatchFloor:         0.12,
		SkipLineCost:       0.9,
		SkipSegCost:        0.7,
		MergePenalty:       0.04,
		ReviewSimilarity:   0.6,
		ExactSimilarity:    0.98,
		TakeSimilarity:     0.55,
		PeelSimilarity:     0.35,
		LineCoverageFloor:  0.35,
		LineSimFloor:       0.15,
		TakeAmbiguity:      0.08,
		ASRConfidenceFloor: 0.5,
		MaxLinesPerBlock:   4,
		MaxSegsPerBlock:    6,
		WordSnapMs:         300,
	}
}

// normLine / normSeg are the normalized working copies.
type normLine struct {
	line ScriptLine
	norm []rune
}
type normSeg struct {
	seg  ASRSegment
	norm []rune
	src  []int // norm pos → rune idx in seg text
	rs   []rune
}

// block is one step of the alignment path: lines[i:i+di), segs[k:k+dk).
// di==0 → unmatched audio run; dk==0 → lines with no audio.
type block struct{ i, di, k, dk int }

// Reconcile aligns the script with the actual ASR transcript and emits a
// reviewable Timeline. Audio is the source of truth: everything spoken
// appears in the output, bound where the match is defensible and flagged
// otherwise; script lines with no audio are listed, never invented.
func Reconcile(script *Script, asr *ASRDoc, opts ReconcileOptions) (*Timeline, error) {
	if opts.MaxLinesPerBlock < 1 {
		opts.MaxLinesPerBlock = 1
	}
	if opts.MaxSegsPerBlock < 1 {
		opts.MaxSegsPerBlock = 1
	}
	lines := make([]normLine, len(script.Lines))
	for i, l := range script.Lines {
		n, _ := normRunes(l.Text)
		lines[i] = normLine{line: l, norm: n}
	}
	// Split fused (multi-sentence) ASR segments at terminal punctuation
	// first — the block matcher caps lines per block, and one 5-minute
	// fused segment would otherwise claim only a handful of lines while
	// the rest look "unread". Splits are idempotent (sentence-level
	// input passes through untouched).
	splitSegs := SplitFusedSegments(asr.Segments)
	segs := make([]normSeg, len(splitSegs))
	for k, s := range splitSegs {
		n, src := normRunes(s.Text)
		segs[k] = normSeg{seg: s, norm: n, src: src, rs: []rune(s.Text)}
	}

	blocks := alignBlocks(lines, segs, opts)

	t := &Timeline{
		Version:    SchemaVersion,
		Audio:      asr.Audio,
		AudioSHA256: asr.AudioSHA256,
		SampleRate: asr.SampleRate,
		DurationMs: asr.DurationMs,
	}
	var unmatchedSegIdx []int
	segEmitted := make([]bool, len(segs))
	lineBound := make([]bool, len(lines))

	for _, b := range blocks {
		switch {
		case b.di == 0: // unmatched audio run
			for k := b.k; k < b.k+b.dk; k++ {
				unmatchedSegIdx = append(unmatchedSegIdx, k)
			}
		case b.dk == 0: // lines with no audio
			for i := b.i; i < b.i+b.di; i++ {
				l := lines[i].line
				t.UnmatchedLines = append(t.UnmatchedLines, UnmatchedLine{
					LineID: l.LineID, SceneID: l.SceneID, RoleID: l.RoleID,
					Text: l.Text, Reason: "no_audio",
				})
				lineBound[i] = true
			}
		default:
			emitBlock(t, lines[b.i:b.i+b.di], segs[b.k:b.k+b.dk], b.i, b.k,
				opts, &unmatchedSegIdx, segEmitted, lineBound)
		}
	}

	// Pass 2: attach repeats. Unmatched segments sufficiently similar to
	// a nearby bound line become candidate takes of it.
	attachTakes(t, lines, segs, unmatchedSegIdx, segEmitted, opts)

	// Finalize take groups: index takes and pick the best per line.
	finalizeTakes(t, lines, opts)

	// Any line still unbound (aligned into a matched block but all its
	// characters absent from the audio) joins unmatched_lines.
	for i, l := range lines {
		if !lineBound[i] {
			l2 := l.line
			t.UnmatchedLines = append(t.UnmatchedLines, UnmatchedLine{
				LineID: l2.LineID, SceneID: l2.SceneID, RoleID: l2.RoleID,
				Text: l2.Text, Reason: "no_audio",
			})
		}
	}

	t.ReorderSortsByTime()
	t.RefreshReviewCount()
	return t, nil
}

// ── block-level DP ──────────────────────────────────────────────

// alignBlocks finds the minimum-cost partition of lines×segs into
// matched/skipped blocks. Costs: match = (1-sim)+MergePenalty*(di-1+dk-1),
// skipped line = SkipLineCost, skipped segment = SkipSegCost.
// oversized reports a segment still holding more text than one block
// can bind — the splitter already failed to cut it (no punctuation,
// inter-sentence pauses below the threshold, or no word ts at all).
// The criterion is the RESIDUAL SIZE, not which features are missing,
// so a fast read with 250ms gaps degrades the same way a wordless
// blob does: the block matcher relaxes the line cap instead of
// reporting mass no_audio.
func oversized(s normSeg, opts ReconcileOptions) bool {
	return len(s.norm) > opts.MaxLinesPerBlock*8
}

// oversizedDiCap bounds how many lines one oversized segment may
// host: ~3 normalized chars per line minimum, hard-capped.
func oversizedDiCap(s normSeg, opts ReconcileOptions) int {
	cap := len(s.norm) / 3
	if cap < opts.MaxLinesPerBlock {
		return opts.MaxLinesPerBlock
	}
	if cap > 64 {
		return 64
	}
	return cap
}

func alignBlocks(lines []normLine, segs []normSeg, opts ReconcileOptions) []block {
	n, m := len(lines), len(segs)
	W := m + 1
	cost := make([]float64, (n+1)*W)
	parent := make([]int32, (n+1)*W) // di<<16 | dk
	for i := range cost {
		cost[i] = 1e18
	}
	cost[0] = 0
	type key struct{ i, di, k, dk int }
	cache := map[key]float64{}

	matchCost := func(i, di, k, dk int) float64 {
		kk := key{i, di, k, dk}
		if c, ok := cache[kk]; ok {
			return c
		}
		sim := blockSimilarity(lines[i:i+di], segs[k:k+dk])
		c := 1e18
		if sim >= opts.MatchFloor || (di == 1 && dk == 1 && sim >= opts.MatchFloor/2) {
			c = (1 - sim) + opts.MergePenalty*float64(di-1+dk-1)
		}
		cache[kk] = c
		return c
	}

	for i := 0; i <= n; i++ {
		for k := 0; k <= m; k++ {
			c := cost[i*W+k]
			if c >= 1e17 {
				continue
			}
			// match blocks
			for dk := 1; dk <= opts.MaxSegsPerBlock && k+dk <= m; dk++ {
				diCap := opts.MaxLinesPerBlock
				if dk == 1 && oversized(segs[k], opts) {
					diCap = oversizedDiCap(segs[k], opts)
				}
				for di := 1; di <= diCap && i+di <= n; di++ {
					nc := c + matchCost(i, di, k, dk)
					cell := (i+di)*W + (k + dk)
					if nc < cost[cell] {
						cost[cell] = nc
						parent[cell] = int32(di<<16 | dk)
					}
				}
			}
			// skip lines
			for di := 1; di <= opts.MaxLinesPerBlock && i+di <= n; di++ {
				nc := c + opts.SkipLineCost*float64(di)
				cell := (i+di)*W + k
				if nc < cost[cell] {
					cost[cell] = nc
					parent[cell] = int32(di << 16)
				}
			}
			// skip segments
			for dk := 1; dk <= opts.MaxSegsPerBlock && k+dk <= m; dk++ {
				nc := c + opts.SkipSegCost*float64(dk)
				cell := i*W + (k + dk)
				if nc < cost[cell] {
					cost[cell] = nc
					parent[cell] = int32(dk)
				}
			}
		}
	}

	var path []block
	for i, k := n, m; i > 0 || k > 0; {
		p := parent[i*W+k]
		di, dk := int(p>>16), int(p&0xffff)
		path = append(path, block{i: i - di, di: di, k: k - dk, dk: dk})
		i, k = i-di, k-dk
	}
	for l, r := 0, len(path)-1; l < r; l, r = l+1, r-1 {
		path[l], path[r] = path[r], path[l]
	}
	return path
}

// blockSimilarity aligns the concatenated normalized text of a block.
func blockSimilarity(ls []normLine, ss []normSeg) float64 {
	var a, b []rune
	for _, l := range ls {
		a = append(a, l.norm...)
	}
	for _, s := range ss {
		b = append(b, s.norm...)
	}
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// F1 over matched chars of the optimal alignment: shared characters
	// count toward similarity while length mismatch lives in the
	// denominator. A pure score/maxLen normalization would zero out
	// legitimate paraphrases like 等下啊×等一下.
	matches := 0
	for _, o := range nwAlign(a, b) {
		if o.op == 'M' {
			matches++
		}
	}
	sim := 2 * float64(matches) / float64(len(a)+len(b))
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

// ── char-level alignment ────────────────────────────────────────

// aop is one alignment operation: 'M' match, 'S' substitute, 'D' delete
// (a char absent in b), 'I' insert (extra b char).
type aop struct {
	op     byte
	ai, bj int
}

// nwAlign runs Needleman-Wunsch (+2 match, -1 mismatch, -2 indel) and
// returns the full op list.
func nwAlign(a, b []rune) []aop {
	n, m := len(a), len(b)
	type cell struct {
		s int
		d byte // 0 diag, 1 up (D: consume a), 2 left (I: consume b)
	}
	dp := make([]cell, (n+1)*(m+1))
	W := m + 1
	for i := 1; i <= n; i++ {
		dp[i*W] = cell{s: dp[(i-1)*W].s - 2, d: 1}
	}
	for j := 1; j <= m; j++ {
		dp[j] = cell{s: dp[j-1].s - 2, d: 2}
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			diag := dp[(i-1)*W+j-1].s
			if a[i-1] == b[j-1] {
				diag += 2
			} else {
				diag -= 1
			}
			up := dp[(i-1)*W+j].s - 2
			left := dp[i*W+j-1].s - 2
			best, dir := diag, byte(0)
			if up > best {
				best, dir = up, 1
			}
			if left > best {
				best, dir = left, 2
			}
			dp[i*W+j] = cell{s: best, d: dir}
		}
	}
	var ops []aop
	for i, j := n, m; i > 0 || j > 0; {
		switch dp[i*W+j].d {
		case 0:
			op := byte('S')
			if a[i-1] == b[j-1] {
				op = 'M'
			}
			ops = append(ops, aop{op: op, ai: i - 1, bj: j - 1})
			i, j = i-1, j-1
		case 1:
			ops = append(ops, aop{op: 'D', ai: i - 1})
			i--
		default:
			ops = append(ops, aop{op: 'I', bj: j - 1})
			j--
		}
	}
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

// nwScore is the NW score without materializing the ops.
func nwScore(a, b []rune) int {
	n, m := len(a), len(b)
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := range prev {
		prev[j] = -2 * j
	}
	for i := 1; i <= n; i++ {
		cur[0] = -2 * i
		for j := 1; j <= m; j++ {
			d := prev[j-1]
			if a[i-1] == b[j-1] {
				d += 2
			} else {
				d--
			}
			cur[j] = max3(d, prev[j]-2, cur[j-1]-2)
		}
		prev, cur = cur, prev
	}
	return prev[m]
}

func max3(a, b, c int) int {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// ── block emission ──────────────────────────────────────────────

// emitBlock aligns one matched block at char level and appends the
// resulting timeline segments. Source segments whose aligned coverage
// is below PeelSimilarity of their own text are reclassified as
// unmatched (they carry mostly foreign audio).
func emitBlock(t *Timeline, bl []normLine, bs []normSeg, i0, k0 int,
	opts ReconcileOptions, unmatched *[]int, segEmitted []bool, lineBound []bool) {

	for _, s := range bs {
		if !oversized(s, opts) {
			continue
		}
		if len(s.seg.Words) > 0 {
			t.Warnings = append(t.Warnings, fmt.Sprintf(
				"%s: 融合段无法自动分句（停顿过短/无标点），内部切点由台本对齐映射到词边界",
				s.seg.SegmentID))
		} else {
			t.Warnings = append(t.Warnings, fmt.Sprintf(
				"%s: 融合段无标点/词时间戳不可切分，按字符对齐降级——内部边界全为估计",
				s.seg.SegmentID))
		}
	}

	// Concat streams with owner maps.
	var A, B []rune
	var aOwner, bOwner []int // local indexes into bl / bs
	for li, l := range bl {
		for range l.norm {
			aOwner = append(aOwner, li)
		}
		A = append(A, l.norm...)
	}
	for si, s := range bs {
		for range s.norm {
			bOwner = append(bOwner, si)
		}
		B = append(B, s.norm...)
	}
	if len(A) == 0 || len(B) == 0 {
		for li := range bl {
			lineBound[i0+li] = true
			l := bl[li].line
			t.UnmatchedLines = append(t.UnmatchedLines, UnmatchedLine{
				LineID: l.LineID, SceneID: l.SceneID, RoleID: l.RoleID,
				Text: l.Text, Reason: "no_audio",
			})
		}
		for si := range bs {
			*unmatched = append(*unmatched, k0+si)
			segEmitted[k0+si] = true
		}
		return
	}

	ops := nwAlign(A, B)

	// Per-seg aligned coverage and per-line aligned spans. aligned counts
	// M+S ops (real audio coverage), matches counts exact 'M' ops only —
	// a substituted character still covers the line but it is not
	// identical, so only matches drive the similarity score.
	segAligned := make([]int, len(bs))
	type lineSpan struct {
		bMin, bMax, aligned, matches int
	}
	spans := make([]lineSpan, len(bl))
	for i := range spans {
		spans[i].bMin = -1
	}
	for _, o := range ops {
		if o.op == 'M' || o.op == 'S' {
			segAligned[bOwner[o.bj]]++
			li := aOwner[o.ai]
			sp := &spans[li]
			if sp.bMin < 0 || o.bj < sp.bMin {
				sp.bMin = o.bj
			}
			if o.bj > sp.bMax {
				sp.bMax = o.bj
			}
			sp.aligned++
			if o.op == 'M' {
				sp.matches++
			}
		}
	}

	// Peel: source segments too little covered become unmatched.
	segKept := make([]bool, len(bs))
	for si, s := range bs {
		kept := true
		if len(s.norm) > 0 && opts.PeelSimilarity > 0 &&
			float64(segAligned[si])/float64(len(s.norm)) < opts.PeelSimilarity {
			kept = false
		}
		segKept[si] = kept
		if !kept {
			*unmatched = append(*unmatched, k0+si)
			segEmitted[k0+si] = true
		}
	}

	// Every B position gets an owner: its aligned line, -2 over peeled
	// segments, else -1 (unassigned insertion). Runs of unassigned
	// positions are absorbed into neighbouring lines — the actual
	// spoken words (trailing particles, stumbles) must survive in the
	// output, never be silently shaved off.
	posOwner := make([]int, len(B))
	for p := range posOwner {
		posOwner[p] = -1
	}
	for _, o := range ops {
		if o.op == 'M' || o.op == 'S' {
			posOwner[o.bj] = aOwner[o.ai]
		}
	}
	for p := range B {
		if posOwner[p] < 0 && !segKept[bOwner[p]] {
			posOwner[p] = -2
		}
	}
	for p := 0; p < len(B); {
		if posOwner[p] != -1 {
			p++
			continue
		}
		q := p
		for q < len(B) && posOwner[q] == -1 {
			q++
		}
		L, R := -2, -2
		for x := p - 1; x >= 0; x-- {
			if posOwner[x] != -1 {
				L = posOwner[x]
				break
			}
		}
		for x := q; x < len(B); x++ {
			if posOwner[x] != -1 {
				R = posOwner[x]
				break
			}
		}
		switch {
		case L >= 0 && R >= 0:
			mid := p + (q-p)/2
			for x := p; x < mid; x++ {
				posOwner[x] = L
			}
			for x := mid; x < q; x++ {
				posOwner[x] = R
			}
		case L >= 0:
			for x := p; x < q; x++ {
				posOwner[x] = L
			}
		case R >= 0:
			for x := p; x < q; x++ {
				posOwner[x] = R
			}
		}
		p = q
	}
	// Effective spans now include the absorbed insertions.
	for p, ow := range posOwner {
		if ow >= 0 {
			sp := &spans[ow]
			if sp.bMin < 0 || p < sp.bMin {
				sp.bMin = p
			}
			if p > sp.bMax {
				sp.bMax = p
			}
		}
	}

	// Word-edge table for snapping interior boundaries.
	segB0 := make([]int, len(bs)) // first B pos of each seg
	{
		pos := 0
		for si, s := range bs {
			segB0[si] = pos
			pos += len(s.norm)
		}
	}
	wordEdge := func(si, bpos int) (ms int, ok bool) {
		// word boundary nearest the B cut bpos (between bpos-1 and bpos)
		s := bs[si]
		p := 0 // norm pos inside seg
		for _, w := range s.seg.Words {
			wn, _ := normRunes(w.Text)
			start := segB0[si] + p
			end := start + len(wn)
			if start == bpos {
				return w.StartMs, true
			}
			if end == bpos {
				return w.EndMs, true
			}
			p += len(wn)
		}
		return 0, false
	}
	// msFor interpolates the time of the B cut lying before position p
	// (p==segB0 → seg.start; p==segB0+len → seg.end). snapped reports the
	// boundary kind.
	msFor := func(si, p int) (int, string) {
		s := bs[si].seg
		L := len(bs[si].norm)
		if L == 0 {
			return s.StartMs, "measured"
		}
		rel := p - segB0[si]
		edgeKind := "measured"
		if s.Coarse {
			// bounds came from chunk duration/token ts, not VAD — an estimate
			edgeKind = "estimated"
		}
		if rel <= 0 {
			return s.StartMs, edgeKind
		}
		if rel >= L {
			return s.EndMs, edgeKind
		}
		if wms, ok := wordEdge(si, p); ok {
			return wms, "word"
		}
		ms := s.StartMs + int(float64(rel)/float64(L)*float64(s.EndMs-s.StartMs)+0.5)
		// snap to a word edge within WordSnapMs
		best, bd := ms, opts.WordSnapMs+1
		for _, w := range s.Words {
			if d := absInt(ms - w.StartMs); d < bd {
				best, bd = w.StartMs, d
			}
			if d := absInt(ms - w.EndMs); d < bd {
				best, bd = w.EndMs, d
			}
		}
		if bd <= opts.WordSnapMs {
			return best, "word"
		}
		return ms, "estimated"
	}

	// For each line, cut its aligned span into per-source-seg pieces.
	segHasPiece := make([]bool, len(bs))
	totalB := 0
	for si := range bs {
		if segB0[si]+len(bs[si].norm) > totalB {
			totalB = segB0[si] + len(bs[si].norm)
		}
	}
	claimed := make([]bool, totalB)
	for li, l := range bl {
		sp := spans[li]
		if sp.bMin < 0 {
			continue // no audio coverage → unmatched_lines later
		}
		emittedAny := false
		spanBLen := sp.bMax - sp.bMin + 1
		sim := 0.0
		if len(l.norm)+spanBLen > 0 {
			sim = 2 * float64(sp.matches) / float64(len(l.norm)+spanBLen)
		}
		// Coverage floor: a skipped line whose characters only match
		// coincidentally must not bind to the audio of a neighbour.
		if len(l.norm) > 0 && opts.LineCoverageFloor > 0 &&
			float64(sp.aligned) < float64(maxInt(2, int(opts.LineCoverageFloor*float64(len(l.norm))+0.5))) {
			continue
		}
		// Similarity floor: all-substitution spans (matches≈0) mean the
		// audio belongs to someone else — peel it so it can become an
		// unmatched segment or a take of the right line.
		if len(l.norm) > 0 && sim < opts.LineSimFloor {
			continue
		}
		status := MatchModified
		if sim >= opts.ExactSimilarity {
			status = MatchExact
		}
		// walk B positions bMin..bMax, grouping into contiguous runs per
		// kept source seg
		p := sp.bMin
		for p <= sp.bMax {
			si := bOwner[p]
			q := p
			for q <= sp.bMax && bOwner[q] == si {
				q++
			}
			if segKept[si] {
				src := bs[si]
				loMs, kindLo := msFor(si, p)
				hiMs, kindHi := msFor(si, q)
				if hiMs <= loMs {
					hiMs = loMs + 1
				}
				kind := "estimated"
				if kindLo == "measured" && kindHi == "measured" {
					kind = "measured"
				} else if kindLo != "estimated" && kindHi != "estimated" {
					kind = "word"
				}
				rawLo := src.src[p-segB0[si]]
				rawHi := src.src[q-1-segB0[si]]
				spoken := strings.TrimSpace(string(src.rs[rawLo : rawHi+1]))
				sid := src.seg.SegmentID
				t.Segments = append(t.Segments, TimelineSegment{
					ASRSegmentID: sid,
					LineID:       l.line.LineID,
					RoleID:       l.line.RoleID,
					ScriptText:   l.line.Text,
					ASRRaw:       spoken,
					SpokenText:   spoken,
					StartMs:      loMs, EndMs: hiMs,
					MatchStatus:  status,
					Similarity:   round3(sim),
					Selected:     true,
					Boundary:     kind,
				})
				emittedAny = true
				segHasPiece[si] = true
				for b := p; b < q; b++ {
					claimed[b] = true
				}
			}
			p = q
		}
		if emittedAny {
			lineBound[i0+li] = true
		}
	}

	// Kept segments that produced no piece (their only line was peeled)
	// route to the unmatched pool so attachTakes can claim them.
	for si := range bs {
		if segKept[si] && !segHasPiece[si] {
			*unmatched = append(*unmatched, k0+si)
			segEmitted[k0+si] = true
		}
	}

	// Leftover coverage: chars of a kept segment no line claimed are
	// still real audio (a fused ASR segment can end mid-utterance).
	// Emit each unclaimed run — similar to a bound line it becomes a
	// retake candidate, else an unmatched fragment — so no audio
	// silently vanishes from the timeline.
	for si := range bs {
		if !segKept[si] {
			continue
		}
		src := bs[si]
		b0, b1 := segB0[si], segB0[si]+len(src.norm)
		for p := b0; p < b1; {
			if claimed[p] {
				p++
				continue
			}
			runStart := p
			for p < b1 && !claimed[p] {
				p++
			}
			q := p
			rawLo := src.src[runStart-segB0[si]]
			rawHi := src.src[q-1-segB0[si]]
			spoken := strings.TrimSpace(string(src.rs[rawLo : rawHi+1]))
			if spoken == "" {
				continue
			}
			loMs, kindLo := msFor(si, runStart)
			hiMs, kindHi := msFor(si, q)
			if hiMs <= loMs {
				hiMs = loMs + 1
			}
			kind := "estimated"
			if kindLo == "measured" && kindHi == "measured" {
				kind = "measured"
			} else if kindLo != "estimated" && kindHi != "estimated" {
				kind = "word"
			}
			fn, _ := normRunes(spoken)
			bestSim, bestLine := 0.0, -1
			for i, l := range bl {
				if s := similarity(fn, l.norm); s > bestSim {
					bestSim, bestLine = s, i
				}
			}
			seg := TimelineSegment{
				ASRSegmentID: src.seg.SegmentID,
				ASRRaw:       spoken,
				SpokenText:   spoken,
				StartMs:      loMs, EndMs: hiMs,
				Boundary:     kind,
				Selected:     false,
			}
			if bestLine >= 0 && bestSim >= opts.TakeSimilarity {
				// looks like another reading of an already-bound line
				l := bl[bestLine]
				seg.LineID, seg.RoleID, seg.ScriptText = l.line.LineID, l.line.RoleID, l.line.Text
				seg.MatchStatus, seg.Similarity = MatchRepeated, round3(bestSim)
			} else {
				seg.MatchStatus = MatchUnmatched
				seg.ReviewRequired = true
				seg.ReviewReasons = []string{ReviewUnmatchedAudio}
			}
			t.Segments = append(t.Segments, seg)
		}
	}

	// Post-emit flags and ids.
	for idx := range t.Segments {
		s := &t.Segments[idx]
		if s.SegmentID == "" {
			base := s.ASRSegmentID
			// count pieces already emitted for this asr segment
			n := 0
			for j := 0; j < idx; j++ {
				if t.Segments[j].ASRSegmentID == base {
					n++
				}
			}
			if n == 0 {
				s.SegmentID = base
			} else {
				s.SegmentID = fmt.Sprintf("%s.%d", base, n+1)
			}
		}
		if s.Boundary == "estimated" {
			s.addReview(ReviewEstimatedBoundary)
		}
		if s.Similarity < opts.ReviewSimilarity {
			s.addReview(ReviewLowSimilarity)
		}
	}
	// ASR confidence flags come from the source segment.
	for idx := range t.Segments {
		s := &t.Segments[idx]
		for _, src := range bs {
			if src.seg.SegmentID == s.ASRSegmentID && src.seg.Confidence > 0 &&
				src.seg.Confidence < opts.ASRConfidenceFloor {
				s.addReview(ReviewLowASRConfidence)
			}
		}
	}
}

func (s *TimelineSegment) addReview(reason string) {
	for _, r := range s.ReviewReasons {
		if r == reason {
			s.ReviewRequired = true
			return
		}
	}
	s.ReviewReasons = append(s.ReviewReasons, reason)
	s.ReviewRequired = true
}

// ── take handling ───────────────────────────────────────────────

// attachTakes binds unmatched segments that look like extra readings of
// a bound line as candidate takes.
func attachTakes(t *Timeline, lines []normLine, segs []normSeg,
	unmatched []int, segEmitted []bool, opts ReconcileOptions) {

	for _, k := range unmatched {
		if segEmitted[k] && emittedSeg(t, segs[k].seg.SegmentID) {
			continue // already appended as unmatched
		}
		// Candidate lines: similar enough to be a retake, or a truncated
		// prefix of the line (a false start — reader stopped, redid it).
		bestSim, bestLine := 0.0, -1
		for i, l := range lines {
			sim := similarity(segs[k].norm, l.norm)
			if sim < opts.TakeSimilarity &&
				!(isPrefixOf(segs[k].norm, l.norm) && len(segs[k].norm) >= 3) {
				continue
			}
			if sim > bestSim {
				bestSim, bestLine = sim, i
			}
		}
		if bestLine < 0 {
			continue
		}
		l := lines[bestLine].line
		s := segs[k].seg
		t.Segments = append(t.Segments, TimelineSegment{
			SegmentID:     s.SegmentID,
			ASRSegmentID:  s.SegmentID,
			LineID:        l.LineID,
			RoleID:        l.RoleID,
			ScriptText:    l.Text,
			ASRRaw:        s.Text,
			SpokenText:    s.Text,
			StartMs:       s.StartMs,
			EndMs:         s.EndMs,
			MatchStatus:   MatchRepeated,
			Similarity:    round3(bestSim),
			Selected:      false,
			Boundary:      "measured",
		})
		segEmitted[k] = true
	}

	// Emit remaining unmatched segments (never dropped).
	for _, k := range unmatched {
		if segEmitted[k] && emittedSeg(t, segs[k].seg.SegmentID) {
			continue
		}
		s := segs[k].seg
		t.Segments = append(t.Segments, TimelineSegment{
			SegmentID:     s.SegmentID,
			ASRSegmentID:  s.SegmentID,
			ASRRaw:        s.Text,
			SpokenText:    s.Text,
			StartMs:       s.StartMs,
			EndMs:         s.EndMs,
			MatchStatus:   MatchUnmatched,
			Selected:      false,
			Boundary:      "measured",
			ReviewRequired: true,
			ReviewReasons:  []string{ReviewUnmatchedAudio},
		})
	}
}

func emittedSeg(t *Timeline, asrID string) bool {
	for _, s := range t.Segments {
		if s.ASRSegmentID == asrID {
			return true
		}
	}
	return false
}

func linePos(lines []normLine, id string) int {
	for i, l := range lines {
		if l.line.LineID == id {
			return i
		}
	}
	return -1
}

// finalizeTakes groups every line's bound segments, indexes candidate
// takes in time order and selects the best one. A member counts as a
// take only if its own text alone resembles the whole line — parts of a
// line split by a pause score low on that test and stay parts.
// Ambiguity is flagged, never silently resolved.
func finalizeTakes(t *Timeline, lines []normLine, opts ReconcileOptions) {
	byLine := map[string][]int{}
	for idx, s := range t.Segments {
		if s.LineID != "" {
			byLine[s.LineID] = append(byLine[s.LineID], idx)
		}
	}
	for _, idxs := range byLine {
		if len(idxs) < 2 {
			continue
		}
		sort.Slice(idxs, func(a, b int) bool {
			return t.Segments[idxs[a]].StartMs < t.Segments[idxs[b]].StartMs
		})
		// solo similarity: this piece's own text vs the whole line.
		solo := make([]float64, len(idxs))
		for j, idx := range idxs {
			li := linePos(lines, t.Segments[idx].LineID)
			n, _ := normRunes(t.Segments[idx].SpokenText)
			if li >= 0 {
				solo[j] = similarity(n, lines[li].norm)
			}
		}
		takeLike := 0
		for j, idx := range idxs {
			s := t.Segments[idx]
			li := linePos(lines, s.LineID)
			prefix := false
			if li >= 0 {
				n, _ := normRunes(s.SpokenText)
				prefix = isPrefixOf(n, lines[li].norm) && len(n) >= 3
			}
			if solo[j] >= opts.TakeSimilarity || s.MatchStatus == MatchRepeated || prefix {
				takeLike++
			}
		}
		if takeLike < 2 {
			continue // genuine parts of one line, not takes
		}
		best, second := -1, -1
		for j := range idxs {
			if best < 0 || solo[j] > solo[best] {
				second = best
				best = j
			} else if second < 0 || solo[j] > solo[second] {
				second = j
			}
		}
		ambiguous := second >= 0 && solo[best]-solo[second] < opts.TakeAmbiguity
		for order, j := range idxs {
			s := &t.Segments[j]
			s.MatchStatus = MatchRepeated
			s.TakeIndex = order + 1
			s.Selected = order == best
			if ambiguous {
				s.addReview(ReviewAmbiguousTakes)
			}
		}
	}

	// Parts of a line split by a pause are not takes: number them
	// part_index 1..N in time order so consumers can tell "one reading
	// in several pieces" apart from "N competing readings".
	for _, idxs := range byLine {
		var parts []int
		for _, idx := range idxs {
			if t.Segments[idx].TakeIndex == 0 {
				parts = append(parts, idx)
			}
		}
		if len(parts) < 2 {
			continue
		}
		sort.Slice(parts, func(a, b int) bool {
			return t.Segments[parts[a]].StartMs < t.Segments[parts[b]].StartMs
		})
		for order, idx := range parts {
			t.Segments[idx].PartIndex = order + 1
		}
	}
}

// isPrefixOf reports whether short is a normalized prefix of long.
func isPrefixOf(short, long []rune) bool {
	if len(short) == 0 || len(short) > len(long) {
		return false
	}
	for i := range short {
		if short[i] != long[i] {
			return false
		}
	}
	return true
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func round3(f float64) float64 {
	return float64(int(f*1000+0.5)) / 1000
}
