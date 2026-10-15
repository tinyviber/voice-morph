package dubbing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"voice-morph/engine"
)

// Rendered manifest schema (rendered/manifest.json). Every artifact a
// dubbing session produces is declared here; nothing is written but
// not recorded, nothing is silently dropped.

type srtCue struct{ a, b int64; text string }

type ManifestLine struct {
	SegmentID   string  `json:"segment_id"`
	LineID      string  `json:"line_id,omitempty"`
	RoleID      string  `json:"role_id"`
	Wav         string  `json:"wav"` // lines/<id>-<key8>.wav, relative to out dir
	SrcWav      string  `json:"src_wav,omitempty"` // lines/src-<id>.wav, unmorphed same window
	CutStartMs  int64   `json:"cut_start_ms"`
	CutEndMs    int64   `json:"cut_end_ms"`
	DurationMs  int64   `json:"duration_ms"`
	CacheHit    bool    `json:"cache_hit"`
	Backend     string  `json:"backend"`
	Experimental bool   `json:"experimental,omitempty"`
}

type ManifestSkip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type ManifestFailure struct {
	SegmentID string `json:"segment_id"`
	Error     string `json:"error"`
}

type Manifest struct {
	Version    int               `json:"version"`
	Audio      string            `json:"audio"`
	AudioSHA256 string           `json:"audio_sha256"`
	SampleRate int               `json:"sample_rate"`
	DurationMs int64             `json:"duration_ms"`
	Status     string            `json:"status"` // complete|blocked_review|partial|failed
	// Counts gives consumers structured reasons without parsing skipped[].
	Counts     map[string]int    `json:"counts,omitempty"`
	Lines      []ManifestLine    `json:"lines"`
	Skipped    []ManifestSkip    `json:"skipped,omitempty"`
	Failures   []ManifestFailure `json:"failures,omitempty"`
	Roles      map[string]string `json:"roles"` // role_id -> roles/<role_id>.wav
	Mix        string            `json:"mix"`
	Subtitles  string            `json:"subtitles"`
	SubtitleJSON string          `json:"subtitle_json"`
	Warnings   []string          `json:"warnings,omitempty"`
	// Extra is an opaque passthrough slot for downstream tooling
	// (set via RenderOptions.Extra); the renderer never interprets it.
	Extra      json.RawMessage   `json:"extra,omitempty"`
}

// LoadManifest reads and checks a rendered manifest.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: invalid JSON: %w", err)
	}
	if m.Version < 1 || m.Version > SchemaVersion {
		return nil, fmt.Errorf("manifest: unsupported version %d", m.Version)
	}
	return &m, nil
}

type RenderOptions struct {
	TimelinePath string
	Timeline     *Timeline // optional: pre-loaded+reviewed timeline
	VoicesPath   string
	AudioPath    string // defaults to timeline.audio
	OutDir       string
	Only         map[string]bool // segment_id or line_id filter; nil = all
	KeepGoing    bool
	Jobs         int
	OnProgress   func(done, total int)
	Extra        json.RawMessage // opaque passthrough into manifest.extra
	// WriteSrc writes lines/src-<segid>.wav — the raw source window next
	// to each rendered line so agents/listeners can A/B without reslicing.
	WriteSrc bool
	// AllowUnreviewed renders segments still flagged review_required
	// (each is recorded in manifest.warnings). Default false: unreviewed
	// anomalies land in skipped instead of silently shipping.
	AllowUnreviewed bool
}

// ctxWarmMs bounds how much leading context we feed the DSP so its
// filters warm up on real same-speaker audio instead of a cold ramp;
// the context is trimmed back out after Render.
const ctxWarmMs = 300
const ctxTailMs = 300
const refinePreMs = 250
const refinePostMs = 150
const minCutMs = 50
const edgeFadeMs = 10
const trackFadeMs = 20

func Render(opts RenderOptions) (*Manifest, error) {
	tl := opts.Timeline
	if tl == nil {
		var err error
		if tl, err = LoadTimeline(opts.TimelinePath); err != nil {
			return nil, fmt.Errorf("timeline: %w", err)
		}
	}
	if err := tl.Validate(); err != nil {
		return nil, fmt.Errorf("timeline invalid: %w", err)
	}
	if err := validateTakeSelection(tl); err != nil {
		return nil, err
	}
	voices, err := LoadVoices(opts.VoicesPath)
	if err != nil {
		return nil, fmt.Errorf("voices: %w", err)
	}
	if err := voices.Validate(); err != nil {
		return nil, fmt.Errorf("voices invalid: %w", err)
	}
	audioPath := opts.AudioPath
	if audioPath == "" {
		audioPath = tl.Audio
	}
	src, err := LoadWAV48(audioPath)
	if err != nil {
		return nil, fmt.Errorf("audio: %w", err)
	}
	// The cache key and manifest always carry the hash of the audio we
	// actually read — a timeline that omits audio_sha256 cannot smuggle
	// a different take past the content-addressed cache.
	actualSHA, _ := SHA256File(audioPath)
	if tl.AudioSHA256 != "" && actualSHA != tl.AudioSHA256 {
		return nil, fmt.Errorf("audio sha256 与 timeline 不符（换了原始录音？）")
	}
	out, err := SessionDir(opts.OutDir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Version: SchemaVersion, Audio: audioPath, AudioSHA256: actualSHA,
		SampleRate: engine.SampleRate, DurationMs: int64(len(src)) * 1000 / engine.SampleRate,
		Roles: map[string]string{}, Mix: "mix.wav", Subtitles: "subtitles.srt", SubtitleJSON: "subtitles.json",
		Status: "complete", Extra: opts.Extra}

	segs := selectedForRender(tl, opts.Only)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].StartMs < segs[j].StartMs })

	// Resolve params + skipped bookkeeping.
	type job struct {
		seg    TimelineSegment
		params engine.Params
		exp    bool
	}
	var jobs []job
	blockedReview, partialLoss := false, false
	for _, s := range segs {
		if s.RoleID == "" || s.MatchStatus == MatchUnmatched {
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID, Reason: "unmatched or no role"})
			partialLoss = true // a selected line the user asked for is missing
			continue
		}
		if s.ReviewRequired && !s.Reviewed && !opts.AllowUnreviewed {
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID,
				Reason: "unreviewed anomaly: " + strings.Join(s.ReviewReasons, ",")})
			blockedReview = true
			continue
		}
		if s.ReviewRequired && !s.Reviewed {
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s: rendered unreviewed (%s)",
				s.SegmentID, strings.Join(s.ReviewReasons, ",")))
		}
		p, err := voices.ParamsFor(s.RoleID)
		if err != nil {
			if opts.KeepGoing {
				m.Failures = append(m.Failures, ManifestFailure{SegmentID: s.SegmentID, Error: err.Error()})
				m.Status = "failed"
				continue
			}
			return nil, fmt.Errorf("role %s: %w", s.RoleID, err)
		}
		jobs = append(jobs, job{seg: s, params: p, exp: voices.Experimental(s.RoleID)})
	}
	for _, s := range tl.Segments {
		switch {
		case s.MatchStatus == MatchUnmatched && s.Discarded:
			// Human explicitly decided not to ship this audio: recorded,
			// no downgrade.
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID,
				Reason: "discarded by reviewer"})
		case s.MatchStatus == MatchUnmatched:
			// Every span of real audio must have a disposition — an
			// undisposed unmatched segment can't leave the manifest and
			// the job can't claim complete; it needs a human verdict
			// (bind / improvise / discard).
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID,
				Reason: "unmatched audio awaiting disposition"})
			blockedReview = true
		case !s.Selected:
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID, Reason: "take not selected"})
		}
	}
	for _, l := range tl.UnmatchedLines {
		m.Skipped = append(m.Skipped, ManifestSkip{ID: l.LineID, Reason: "script line has no audio"})
		if !l.Optional {
			// A script line that was never read is a real gap in the
			// deliverable — complete would silently ship a video
			// missing dialogue. Only an explicit optional marker
			// excuses it.
			partialLoss = true
		}
	}

	// Boundary refinement is sequential so neighbors never overlap.
	env := energyRMS(src, engine.SampleRate*20/1000, engine.SampleRate*5/1000)
	hop := engine.SampleRate * 5 / 1000
	type cut struct{ a, b int }
	cuts := make([]cut, len(jobs))
	prevEnd := 0
	for i, j := range jobs {
		lo := prevEnd
		a := refineBoundary(env, hop, MsToSamples(int(j.seg.StartMs), len(src)),
			MsToSamples(refinePreMs, len(src)), MsToSamples(refinePostMs, len(src)), lo, len(src))
		b := refineBoundary(env, hop, MsToSamples(int(j.seg.EndMs), len(src)),
			MsToSamples(refinePostMs, len(src)), MsToSamples(refinePreMs, len(src)), a+MsToSamples(minCutMs, len(src)), len(src))
		cuts[i] = cut{a, b}
		prevEnd = b
	}

	// Parallel per-segment render; each segment gets a fresh engine so
	// no DSP state ever bleeds across roles. Content-addressed cache:
	// a lines/<seg>-<key8>.wav that already decodes to the expected
	// length is reused verbatim — parameters or cuts change the key.
	results := make([][]float32, len(jobs))
	cacheHit := make([]bool, len(jobs))
	wavNames := make([]string, len(jobs))
	for i, j := range jobs {
		key := cacheKey(j.seg, cuts[i], actualSHA, j.params)
		wavNames[i] = fmt.Sprintf("lines/%s-%s.wav", safeFileName(j.seg.SegmentID), key[:8])
	}
	if opts.Jobs < 1 {
		opts.Jobs = runtime.NumCPU()
	}
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	fail := func(i int, err error) {
		mu.Lock()
		defer mu.Unlock()
		m.Failures = append(m.Failures, ManifestFailure{SegmentID: jobs[i].seg.SegmentID, Error: err.Error()})
		m.Status = "failed"
	}
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := cuts[i]
			// Cache hit: reuse the decoded product, still verify it.
			if x, err := LoadWAV48(joinOut(out, wavNames[i])); err == nil && len(x) == c.b-c.a {
				results[i] = x
				cacheHit[i] = true
				mu.Lock()
				done++
				if opts.OnProgress != nil {
					opts.OnProgress(done, len(jobs))
				}
				mu.Unlock()
				return
			}
			ctxL := minInt(MsToSamples(ctxWarmMs, len(src)), c.a)
			ctxR := minInt(MsToSamples(ctxTailMs, len(src)), len(src)-c.b)
			window := slice(src, c.a-ctxL, c.b+ctxR)
			applyEdgeFades(window, MsToSamples(edgeFadeMs, len(src)))
			eng := engine.New()
			eng.SetParams(j.params)
			out := eng.Render(window) // len(out) == len(window) (engine contract)
			results[i] = slice(out, ctxL, ctxL+c.b-c.a)
			mu.Lock()
			done++
			if opts.OnProgress != nil {
				opts.OnProgress(done, len(jobs))
			}
			mu.Unlock()
		}(i, j)
	}
	wg.Wait()

	// Write per-line WAVs (content-addressed), build role tracks.
	roleBuf := map[string][]float32{}
	filled := map[string][]bool{}
	var cueList []map[string]any
	var cues []srtCue
	for i, j := range jobs {
		if m.Status == "failed" && !opts.KeepGoing && len(m.Failures) > 0 {
			break
		}
		if results[i] == nil {
			continue
		}
		wavName := wavNames[i]
		if !cacheHit[i] {
			if err := SaveWAV(joinOut(out, wavName), results[i]); err != nil {
			if opts.KeepGoing {
				fail(i, err)
				continue
			}
			return nil, err
			}
		}
		srcName := ""
		if opts.WriteSrc {
			srcName = fmt.Sprintf("lines/src-%s.wav", uniqueFileName(j.seg.SegmentID))
			if err := SaveWAV(joinOut(out, srcName), slice(src, cuts[i].a, cuts[i].b)); err != nil {
				m.Warnings = append(m.Warnings, srcName+": "+err.Error())
				srcName = ""
			}
		}
		aMs, bMs := int64(cuts[i].a)*1000/engine.SampleRate, int64(cuts[i].b)*1000/engine.SampleRate
		m.Lines = append(m.Lines, ManifestLine{
			SegmentID: j.seg.SegmentID, LineID: j.seg.LineID, RoleID: j.seg.RoleID,
			Wav: wavName, SrcWav: srcName, CutStartMs: aMs, CutEndMs: bMs, DurationMs: bMs - aMs,
			CacheHit: cacheHit[i], Backend: "dsp", Experimental: j.exp})
		if _, ok := roleBuf[j.seg.RoleID]; !ok {
			roleBuf[j.seg.RoleID] = make([]float32, len(src))
			filled[j.seg.RoleID] = make([]bool, len(src))
		}
		crossfadeInto(roleBuf[j.seg.RoleID], results[i], cuts[i].a, MsToSamples(trackFadeMs, len(src)), filled[j.seg.RoleID])
		cues = append(cues, srtCue{aMs, bMs, j.seg.SpokenText})
		cueList = append(cueList, map[string]any{
			"segment_id": j.seg.SegmentID, "line_id": j.seg.LineID, "role_id": j.seg.RoleID,
			"start_ms": aMs, "end_ms": bMs, "text": j.seg.SpokenText})
	}
	if len(m.Failures) > 0 && !opts.KeepGoing {
		return m, fmt.Errorf("render failed: %s", m.Failures[0].Error)
	}

	// Role tracks + mix.
	mixBuf := make([]float32, len(src))
	mixFilled := make([]bool, len(src))
	var roleIDs []string
	for r := range roleBuf {
		roleIDs = append(roleIDs, r)
	}
	sort.Strings(roleIDs)
	for _, r := range roleIDs {
		name := "roles/" + uniqueFileName(r) + ".wav"
		if err := SaveWAV(joinOut(out, name), roleBuf[r]); err != nil {
			return nil, err
		}
		m.Roles[r] = name
	}
	// Mix: every rendered line placed at its cut position.
	for i := range jobs {
		if results[i] != nil {
			crossfadeInto(mixBuf, results[i], cuts[i].a, MsToSamples(trackFadeMs, len(src)), mixFilled)
		}
	}
	if p := peakOf(mixBuf); p > 1 {
		g := 0.98 / p
		for i := range mixBuf {
			mixBuf[i] *= float32(g)
		}
		m.Warnings = append(m.Warnings, fmt.Sprintf("mix peak %.2f → gain %.3f", p, g))
	}
	if err := SaveWAV(joinOut(out, "mix.wav"), mixBuf); err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(joinOut(out, "subtitles.srt"), []byte(formatSRT(cues)), 0o644); err != nil {
		return nil, err
	}
	if b, err := json.MarshalIndent(cueList, "", "  "); err == nil {
		if err := WriteFileAtomic(joinOut(out, "subtitles.json"), append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	// Validation pass: every written WAV decodes and matches declared length.
	for _, ln := range m.Lines {
		x, err := LoadWAV48(joinOut(out, ln.Wav))
		if err != nil {
			m.Warnings = append(m.Warnings, ln.Wav+": unreadable: "+err.Error())
			continue
		}
		want := int(ln.DurationMs) * engine.SampleRate / 1000
		if len(x) != want {
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s: len %d != declared %dms", ln.Wav, len(x), ln.DurationMs))
		}
		if p := peakOf(x); p > 0.995 {
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s: peak %.3f (post-gain clipping)", ln.Wav, p))
		}
	}
	// Status semantics an automated consumer can trust: "complete" only
	// when every selected line rendered AND no required script line is
	// missing audio; "partial" outranks "blocked_review" — when both
	// apply, review alone won't close the gap.
	m.Counts = map[string]int{
		"rendered": len(m.Lines), "failures": len(m.Failures),
		"blocked_review": 0, "missing_lines": 0, "unselected_takes": 0,
		"undisposed_audio": 0, "discarded": 0,
	}
	for _, s := range m.Skipped {
		switch {
		case strings.HasPrefix(s.Reason, "unreviewed anomaly"):
			m.Counts["blocked_review"]++
		case s.Reason == "script line has no audio":
			m.Counts["missing_lines"]++
		case s.Reason == "take not selected":
			m.Counts["unselected_takes"]++
		case s.Reason == "unmatched audio awaiting disposition":
			m.Counts["undisposed_audio"]++
		case s.Reason == "discarded by reviewer":
			m.Counts["discarded"]++
		}
	}
	if m.Status != "failed" {
		switch {
		case partialLoss:
			m.Status = "partial"
		case blockedReview:
			m.Status = "blocked_review"
		default:
			m.Status = "complete"
		}
	}
	return m, SaveJSON(joinOut(out, "manifest.json"), m)
}

func cacheKey(seg TimelineSegment, c struct{ a, b int }, audioSHA string, p engine.Params) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d|%s|%.6f|%.6f|%.6f|%.4f|%v|%v|%.4f|%.4f|%v|%v|engine-v2",
		seg.SegmentID, c.a, c.b, audioSHA, p.Pitch, p.Timbre, p.Strength, p.Gain,
		p.Bypass, p.Speech, p.PitchRange, p.TargetF0, p.EqPre, p.EqPost)
	fmt.Fprintf(h, "|%.6f", p.FormantShape)
	return hex.EncodeToString(h.Sum(nil))
}

// uniqueFileName is the collision-proof name for a raw id: sanitized
// form + hash of the RAW id, ALWAYS appended — sanitized names collide
// on cleaned characters ("hero."→"hero") and on case-insensitive
// filesystems ("Narrator"/"narrator"). Use for role tracks and src
// windows; rendered line wavs stay collision-safe through the
// per-content key suffix.
func uniqueFileName(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%s-%x", safeFileName(s), h[:4])
}

// safeFileName maps an arbitrary id to a unique filesystem-safe
// component: alnum plus -_. pass through; everything else collapses to
// _. Whenever characters were folded (non-Latin ids like 悟空/布玛 would
// otherwise collide at "__"), the original id's hash is appended so
// distinct ids still map to distinct files. Path traversal through
// crafted role/segment ids cannot escape the output dir.
func safeFileName(id string) string {
	b := strings.Builder{}
	b.Grow(len(id))
	folded := false
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			folded = true
		}
	}
	s := b.String()
	s = strings.Trim(s, ".")
	if folded {
		h := sha256.Sum256([]byte(id))
		if s == "" || strings.Trim(s, "_") == "" {
			s = "f-" + hex.EncodeToString(h[:])[:8]
		} else {
			s = s + "-" + hex.EncodeToString(h[:])[:8]
		}
	}
	if s == "" {
		s = "_"
	}
	return s
}

// joinOut joins a path inside the output dir and refuses any rel path
// that would escape it (belts on top of safeFileName'd components).
func joinOut(dir, rel string) string {
	p := filepath.Clean(filepath.Join(dir, rel))
	base := filepath.Clean(dir) + string(os.PathSeparator)
	if p != filepath.Clean(dir) && !strings.HasPrefix(p, base) {
		// Fall back to a sanitized flat name rather than escaping.
		p = filepath.Join(dir, safeFileName(rel))
	}
	return p
}

func selectedForRender(tl *Timeline, only map[string]bool) []TimelineSegment {
	var out []TimelineSegment
	for _, s := range tl.Segments {
		if !s.Selected {
			continue
		}
		if only != nil && !only[s.SegmentID] && !only[s.LineID] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func validateTakeSelection(tl *Timeline) error {
	type lineSelection struct {
		take    string
		nonTake string
	}
	selected := make(map[string]lineSelection)
	for _, s := range tl.Segments {
		if !s.Selected || s.LineID == "" {
			continue
		}
		choice := selected[s.LineID]
		if s.TakeIndex > 0 {
			if choice.take != "" {
				return fmt.Errorf("line %s has multiple selected takes: %s and %s", s.LineID, choice.take, s.SegmentID)
			}
			if choice.nonTake != "" {
				return fmt.Errorf("line %s selects take %s alongside segment %s", s.LineID, s.SegmentID, choice.nonTake)
			}
			choice.take = s.SegmentID
		} else {
			if choice.take != "" {
				return fmt.Errorf("line %s selects take %s alongside segment %s", s.LineID, choice.take, s.SegmentID)
			}
			if choice.nonTake == "" {
				choice.nonTake = s.SegmentID
			}
		}
		selected[s.LineID] = choice
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatSRT(cues []srtCue) string {
	var s string
	for i, c := range cues {
		s += fmt.Sprintf("%d\n%s --> %s\n%s\n\n", i+1, srtTime(c.a), srtTime(c.b), c.text)
	}
	return s
}

func srtTime(ms int64) string {
	h := ms / 3600000
	m := ms / 60000 % 60
	s := ms / 1000 % 60
	x := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, x)
}
