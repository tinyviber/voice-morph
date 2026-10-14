package dubbing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
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
	Status     string            `json:"status"` // "ok"|"failed"
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
	if sha, _ := SHA256File(audioPath); tl.AudioSHA256 != "" && sha != tl.AudioSHA256 {
		return nil, fmt.Errorf("audio sha256 与 timeline 不符（换了原始录音？）")
	}
	out, err := SessionDir(opts.OutDir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Version: SchemaVersion, Audio: audioPath, AudioSHA256: tl.AudioSHA256,
		SampleRate: engine.SampleRate, DurationMs: int64(len(src)) * 1000 / engine.SampleRate,
		Roles: map[string]string{}, Mix: "mix.wav", Subtitles: "subtitles.srt", SubtitleJSON: "subtitles.json",
		Status: "ok", Extra: opts.Extra}

	segs := selectedForRender(tl, opts.Only)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].StartMs < segs[j].StartMs })

	// Resolve params + skipped bookkeeping.
	type job struct {
		seg    TimelineSegment
		params engine.Params
		exp    bool
	}
	var jobs []job
	for _, s := range segs {
		if s.RoleID == "" || s.MatchStatus == MatchUnmatched {
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID, Reason: "unmatched or no role"})
			continue
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
		if !s.Selected && s.MatchStatus != MatchUnmatched {
			m.Skipped = append(m.Skipped, ManifestSkip{ID: s.SegmentID, Reason: "take not selected"})
		}
	}
	for _, l := range tl.UnmatchedLines {
		m.Skipped = append(m.Skipped, ManifestSkip{ID: l.LineID, Reason: "script line has no audio"})
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
	// no DSP state ever bleeds across roles.
	results := make([][]float32, len(jobs))
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
		key := cacheKey(j.seg, cuts[i], tl.AudioSHA256, j.params)
		wavName := fmt.Sprintf("lines/%s-%s.wav", j.seg.SegmentID, key[:8])
		if err := SaveWAV(joinOut(out, wavName), results[i]); err != nil {
			if opts.KeepGoing {
				fail(i, err)
				continue
			}
			return nil, err
		}
		srcName := ""
		if opts.WriteSrc {
			srcName = fmt.Sprintf("lines/src-%s.wav", j.seg.SegmentID)
			if err := SaveWAV(joinOut(out, srcName), slice(src, cuts[i].a, cuts[i].b)); err != nil {
				m.Warnings = append(m.Warnings, srcName+": "+err.Error())
				srcName = ""
			}
		}
		aMs, bMs := int64(cuts[i].a)*1000/engine.SampleRate, int64(cuts[i].b)*1000/engine.SampleRate
		m.Lines = append(m.Lines, ManifestLine{
			SegmentID: j.seg.SegmentID, LineID: j.seg.LineID, RoleID: j.seg.RoleID,
			Wav: wavName, SrcWav: srcName, CutStartMs: aMs, CutEndMs: bMs, DurationMs: bMs - aMs,
			Backend: "dsp", Experimental: j.exp})
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
		name := "roles/" + r + ".wav"
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
	return m, SaveJSON(joinOut(out, "manifest.json"), m)
}

func cacheKey(seg TimelineSegment, c struct{ a, b int }, audioSHA string, p engine.Params) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d|%s|%.6f|%.6f|%.6f|%.4f|%v|%v|%.4f|%s|engine-v1",
		seg.SegmentID, c.a, c.b, audioSHA, p.Pitch, p.Timbre, p.Strength, p.Gain,
		p.Bypass, p.Speech, p.PitchRange, fmt.Sprintf("%.3f", p.FormantShape))
	return hex.EncodeToString(h.Sum(nil))
}

func joinOut(dir, rel string) string {
	return dir + "/" + rel
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
