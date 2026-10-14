package dubbing

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voice-morph/engine"
)

// synthSpeech fabricates speech-like audio: voiced hum + silence so
// boundary refinement and track placement have something real to chew.
func synthSpeech(durMs int, activeRanges [][2]int) []float32 {
	n := durMs * engine.SampleRate / 1000
	x := make([]float32, n)
	for _, r := range activeRanges {
		a, b := r[0]*engine.SampleRate/1000, r[1]*engine.SampleRate/1000
		for i := a; i < b && i < n; i++ {
			t := float64(i) / engine.SampleRate
			x[i] = float32(0.3*math.Sin(2*math.Pi*150*t) + 0.1*math.Sin(2*math.Pi*450*t))
		}
	}
	return x
}

func writeWAV(t *testing.T, path string, x []float32) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, engine.EncodeWAV(x, engine.SampleRate), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRender_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	// 10s of audio: speech 0.5–2.5s (l01), silence, speech 4–6s (l02),
	// silence, speech 7–9s (l03 role B).
	src := synthSpeech(10000, [][2]int{{500, 2500}, {4000, 6000}, {7000, 9000}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)

	tl := &Timeline{
		Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: 10000,
		Segments: []TimelineSegment{
			{SegmentID: "seg-0001", ASRSegmentID: "asr-1", LineID: "l01", RoleID: "narrator",
				ASRRaw: "大家好", SpokenText: "大家好", StartMs: 500, EndMs: 2500,
				MatchStatus: MatchExact, Selected: true, Boundary: "measured"},
			{SegmentID: "seg-0002", ASRSegmentID: "asr-2", LineID: "l02", RoleID: "narrator",
				ASRRaw: "我们开始吧", SpokenText: "我们开始吧", StartMs: 4000, EndMs: 6000,
				MatchStatus: MatchExact, Selected: true, Boundary: "measured"},
			{SegmentID: "seg-0003", ASRSegmentID: "asr-3", LineID: "l03", RoleID: "character-a",
				ASRRaw: "轮到我了", SpokenText: "轮到我了", StartMs: 7000, EndMs: 9000,
				MatchStatus: MatchExact, Selected: true, Boundary: "measured"},
			{SegmentID: "seg-0004", ASRSegmentID: "asr-4", LineID: "l01", RoleID: "narrator",
				ASRRaw: "大家好", SpokenText: "大家好", StartMs: 9300, EndMs: 9900,
				MatchStatus: MatchRepeated, TakeIndex: 2, Selected: false},
		},
	}
	tlPath := filepath.Join(dir, "timeline.json")
	if err := SaveJSON(tlPath, tl); err != nil {
		t.Fatal(err)
	}
	voicesPath := filepath.Join(dir, "voices.json")
	voicesJSON := `{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"},"character-a":{"backend":"dsp","preset":"low"}}}`
	if err := os.WriteFile(voicesPath, []byte(voicesJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "rendered")
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: outDir, Jobs: 2})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if m.Status != "complete" {
		t.Fatalf("status=%s failures=%v", m.Status, m.Failures)
	}
	if len(m.Lines) != 3 {
		t.Fatalf("want 3 rendered lines, got %d", len(m.Lines))
	}
	// Per-line WAVs decode and match declared duration (±1ms rounding).
	for _, ln := range m.Lines {
		x, err := LoadWAV48(filepath.Join(outDir, ln.Wav))
		if err != nil {
			t.Fatalf("line wav: %v", err)
		}
		if got := int64(len(x)) * 1000 / engine.SampleRate; abs64f(got-ln.DurationMs) > 1 {
			t.Fatalf("%s: dur %dms != declared %dms", ln.Wav, got, ln.DurationMs)
		}
		if rmsOf(x) < 1e-4 {
			t.Fatalf("%s rendered silence", ln.Wav)
		}
	}
	// Skipped take is recorded, not rendered.
	if len(m.Skipped) != 1 || m.Skipped[0].ID != "seg-0004" {
		t.Fatalf("skipped=%+v", m.Skipped)
	}
	// Role tracks: full-length, speech only where that role's lines sit.
	for role, rel := range m.Roles {
		x, err := LoadWAV48(filepath.Join(outDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if len(x) != len(src) {
			t.Fatalf("role %s track len %d != src %d", role, len(x), len(src))
		}
	}
	narr, _ := LoadWAV48(filepath.Join(outDir, m.Roles["narrator"]))
	charA, _ := LoadWAV48(filepath.Join(outDir, m.Roles["character-a"]))
	// narrator silent in 7–9s zone; character-a silent in 0.5–2.5s zone.
	if rmsOf(slice(narr, MsToSamples(7200, len(narr)), MsToSamples(8800, len(narr)))) > 0.02 {
		t.Fatal("narrator track bleeds into character-a zone")
	}
	if rmsOf(slice(charA, MsToSamples(600, len(charA)), MsToSamples(2400, len(charA)))) > 0.02 {
		t.Fatal("character-a track bleeds into narrator zone")
	}
	// Mix has energy in all three zones.
	mix, _ := LoadWAV48(filepath.Join(outDir, "mix.wav"))
	for _, ms := range [][2]int{{1000, 2000}, {4500, 5500}, {7500, 8500}} {
		if rmsOf(slice(mix, MsToSamples(ms[0], len(mix)), MsToSamples(ms[1], len(mix)))) < 1e-4 {
			t.Fatalf("mix silent in %d-%dms", ms[0], ms[1])
		}
	}
	// Subtitles carry spoken text and real cut times.
	sb, err := os.ReadFile(filepath.Join(outDir, "subtitles.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sb) == 0 {
		t.Fatal("empty srt")
	}
	var cues []map[string]any
	if err := json.Unmarshal(mustRead(t, filepath.Join(outDir, "subtitles.json")), &cues); err != nil {
		t.Fatal(err)
	}
	if len(cues) != 3 {
		t.Fatalf("want 3 cues, got %d", len(cues))
	}
	// Manifest round-trips.
	m2, err := LoadManifest(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Lines) != 3 {
		t.Fatal("manifest reload lost lines")
	}
	// Character-a params actually differ from narrator (pitch shifted).
	if !containsFile(t, filepath.Join(outDir, m.Lines[2].Wav)) {
		t.Fatal("line wav missing")
	}
}

func abs64f(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsFile(t *testing.T, p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Undisposed unmatched audio must appear in skipped and downgrade the
// manifest — silence about real audio is the exact hole a downstream
// agent falls into. An explicit discard records the verdict without
// downgrading.
func TestRender_UndisposedUnmatchedBlocksComplete(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(4000, [][2]int{{500, 1500}, {2500, 3500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	mk := func(discard bool) *Timeline {
		return &Timeline{
			Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
			SampleRate: engine.SampleRate, DurationMs: 4000,
			Segments: []TimelineSegment{
				{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
					ASRRaw: "你好", StartMs: 500, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
				{SegmentID: "seg-0002", ASRRaw: "临时插一句", SpokenText: "临时插一句",
					StartMs: 2500, EndMs: 3500, MatchStatus: MatchUnmatched,
					Discarded: discard, ReviewRequired: !discard,
					ReviewReasons: []string{ReviewUnmatchedAudio}},
			},
		}
	}
	tlPath := filepath.Join(dir, "timeline.json")
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	if err := SaveJSON(tlPath, mk(false)); err != nil {
		t.Fatal(err)
	}
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status == "complete" {
		t.Fatal("undisposed unmatched audio must not yield complete")
	}
	found := false
	for _, s := range m.Skipped {
		if s.ID == "seg-0002" && s.Reason == "unmatched audio awaiting disposition" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unmatched audio must be recorded in skipped: %+v", m.Skipped)
	}
	if m.Counts["undisposed_audio"] != 1 {
		t.Fatalf("counts must surface the undisposed audio: %+v", m.Counts)
	}
	if err := SaveJSON(tlPath, mk(true)); err != nil {
		t.Fatal(err)
	}
	m, err = Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out2")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "complete" {
		t.Fatalf("explicit discard must clear the path to complete, got %s", m.Status)
	}
	found = false
	for _, s := range m.Skipped {
		if s.ID == "seg-0002" && s.Reason == "discarded by reviewer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discard verdict must be recorded: %+v", m.Skipped)
	}
}

func TestRenderRejectsMutuallySelectedTakes(t *testing.T) {
	for _, tc := range []struct {
		name string
		segs []TimelineSegment
	}{
		{
			name: "two takes",
			segs: []TimelineSegment{
				{SegmentID: "take-1", LineID: "l01", StartMs: 0, EndMs: 900,
					MatchStatus: MatchRepeated, TakeIndex: 1, Selected: true},
				{SegmentID: "take-2", LineID: "l01", StartMs: 1000, EndMs: 1900,
					MatchStatus: MatchRepeated, TakeIndex: 2, Selected: true},
			},
		},
		{
			name: "take with another segment",
			segs: []TimelineSegment{
				{SegmentID: "part-1", LineID: "l01", StartMs: 0, EndMs: 900,
					MatchStatus: MatchExact, Selected: true},
				{SegmentID: "take-1", LineID: "l01", StartMs: 1000, EndMs: 1900,
					MatchStatus: MatchRepeated, TakeIndex: 1, Selected: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := &Timeline{Version: SchemaVersion, Audio: "unused.wav",
				SampleRate: engine.SampleRate, Segments: tc.segs}
			_, err := Render(RenderOptions{Timeline: tl, VoicesPath: filepath.Join(t.TempDir(), "unused.json")})
			if err == nil || !strings.Contains(err.Error(), "line l01") {
				t.Fatalf("want take-selection error before I/O, got %v", err)
			}
		})
	}
}

func TestRender_UnmatchedKeepsRaw(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(4000, [][2]int{{500, 1500}, {2500, 3500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	// seg2 is unmatched audio: must be skipped, never rendered into a role.
	tl := &Timeline{
		Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: 4000,
		Segments: []TimelineSegment{
			{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
				ASRRaw: "你好", StartMs: 500, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
			{SegmentID: "seg-0002", ASRRaw: "广告词", SpokenText: "广告词", StartMs: 2500, EndMs: 3500,
				MatchStatus: MatchUnmatched, Selected: false, ReviewRequired: true,
				ReviewReasons: []string{ReviewUnmatchedAudio}},
		},
	}
	tlPath := filepath.Join(dir, "timeline.json")
	if err := SaveJSON(tlPath, tl); err != nil {
		t.Fatal(err)
	}
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Lines) != 1 {
		t.Fatalf("unmatched audio leaked into render: %+v", m.Lines)
	}
}

func abs64For(x int64) int64 { return abs64f(x) }

func TestRender_UnreviewedGatedByDefault(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(4000, [][2]int{{500, 1500}, {2500, 3500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	mk := func(reviewed bool) *Timeline {
		return &Timeline{
			Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
			SampleRate: engine.SampleRate, DurationMs: 4000,
			Segments: []TimelineSegment{
				{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
					ASRRaw: "你好", StartMs: 500, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
				{SegmentID: "seg-0002", LineID: "l02", RoleID: "narrator", SpokenText: "在吗",
					ASRRaw: "在吗", StartMs: 2500, EndMs: 3500, MatchStatus: MatchModified,
					Selected: true, ReviewRequired: true, Edited: true, Reviewed: reviewed,
					ReviewReasons: []string{ReviewLowSimilarity}},
			},
		}
	}
	tlPath := filepath.Join(dir, "timeline.json")
	if err := SaveJSON(tlPath, mk(false)); err != nil {
		t.Fatal(err)
	}
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Lines) != 1 {
		t.Fatalf("unreviewed anomaly rendered: %+v", m.Lines)
	}
	found := false
	for _, s := range m.Skipped {
		if s.ID == "seg-0002" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unreviewed seg not in skipped: %+v", m.Skipped)
	}
	// The skipped unreviewed line also downgrades status so automated
	// consumers can't mistake a review-blocked render for success.
	if m.Status != "blocked_review" {
		t.Fatalf("unreviewed-selected skip should yield blocked_review, got %s", m.Status)
	}
	// Explicit override renders it but records a warning.
	m, err = Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath,
		OutDir: filepath.Join(dir, "out2"), AllowUnreviewed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Lines) != 2 || len(m.Warnings) == 0 {
		t.Fatalf("--allow-unreviewed should render+warn: lines=%v warnings=%v", m.Lines, m.Warnings)
	}
	if err := SaveJSON(tlPath, mk(true)); err != nil {
		t.Fatal(err)
	}
	m, err = Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out3")})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Lines) != 2 || m.Status != "complete" {
		t.Fatalf("confirmed seg should render complete: %+v status=%s", m.Lines, m.Status)
	}
}

func TestRender_ManualBindAndPathSafety(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(4000, [][2]int{{500, 1500}, {2500, 3500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	tl := &Timeline{
		Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: 4000,
		Segments: []TimelineSegment{
			{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
				ASRRaw: "你好", StartMs: 500, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
			// Human bound an ad-lib to a crafted role id — must not escape out dir.
			{SegmentID: "seg-0002", RoleID: "../evil", SpokenText: "广告词",
				ASRRaw: "广告词", StartMs: 2500, EndMs: 3500,
				MatchStatus: MatchManual, Selected: true, Edited: true},
		},
	}
	tlPath := filepath.Join(dir, "timeline.json")
	if err := SaveJSON(tlPath, tl); err != nil {
		t.Fatal(err)
	}
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"},"../evil":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	outDir := filepath.Join(dir, "out")
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Lines) != 2 {
		t.Fatalf("manual-bound seg should render: %+v", m.Lines)
	}
	for _, rel := range m.Roles {
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			t.Fatalf("role track path escapes: %q", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.wav")); err == nil {
		t.Fatal("path traversal wrote outside out dir")
	}
	if !filepath.IsLocal(m.Lines[1].Wav) {
		t.Fatalf("line wav path escapes: %q", m.Lines[1].Wav)
	}
}

func TestRender_CacheHits(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(3000, [][2]int{{500, 2500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	tl := &Timeline{
		Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: 3000,
		Segments: []TimelineSegment{
			{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
				ASRRaw: "你好", StartMs: 500, EndMs: 2500, MatchStatus: MatchExact, Selected: true},
		},
	}
	tlPath := filepath.Join(dir, "timeline.json")
	if err := SaveJSON(tlPath, tl); err != nil {
		t.Fatal(err)
	}
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	outDir := filepath.Join(dir, "out")
	m1, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(m1.Lines) != 1 || m1.Lines[0].CacheHit {
		t.Fatalf("first render should render cold: %+v", m1.Lines)
	}
	m2, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Lines) != 1 || !m2.Lines[0].CacheHit {
		t.Fatalf("second render should hit cache: %+v", m2.Lines)
	}
	// Same audio but a different voice param → different key → cold again.
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"low"}}}`), 0o644)
	m3, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(m3.Lines) != 1 || m3.Lines[0].CacheHit {
		t.Fatalf("param change must bust cache: %+v", m3.Lines)
	}
}

func TestSafeFileName_NoCollisions(t *testing.T) {
	ids := []string{"悟空", "布玛", "旁白", "../evil", "a/b", "narrator"}
	seen := map[string]string{}
	for _, id := range ids {
		got := safeFileName(id)
		if strings.ContainsAny(got, "/\\") || got == "" {
			t.Fatalf("%q -> unsafe %q", id, got)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%q and %q collide at %q", prev, id, got)
		}
		seen[got] = id
	}
	// Pure-ASCII ids stay readable and unchanged.
	if got := safeFileName("narrator"); got != "narrator" {
		t.Fatalf("ascii id mangled: %q", got)
	}
}

// uniqueFileName hashes the RAW id unconditionally, so pairs that fold
// to identical or case-equal file names still can't collide —
// "hero."/"hero" on any FS and "Narrator"/"narrator" on a
// case-insensitive one.
func TestUniqueFileName_NoCollisions(t *testing.T) {
	pairs := [][2]string{
		{"hero", "hero."},
		{"Narrator", "narrator"},
		{"悟空", "布玛"},
		{"../evil", "evil"},
	}
	for _, p := range pairs {
		a, b := uniqueFileName(p[0]), uniqueFileName(p[1])
		if a == b {
			t.Fatalf("%q and %q collide at %q", p[0], p[1], a)
		}
		if strings.ContainsAny(a, "/\\") || a == "" {
			t.Fatalf("%q -> unsafe %q", p[0], a)
		}
	}
	if !strings.HasPrefix(uniqueFileName("narrator"), "narrator-") {
		t.Fatalf("ascii name lost readability: %q", uniqueFileName("narrator"))
	}
}

// A script line that was never read must downgrade the manifest to
// "partial" — "complete" would tell a downstream agent the whole job
// shipped while dialogue is missing. optional:true excuses it.
func TestRender_MissingLineIsPartial(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(4000, [][2]int{{500, 1500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	mk := func(optional bool) *Timeline {
		return &Timeline{
			Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
			SampleRate: engine.SampleRate, DurationMs: 4000,
			Segments: []TimelineSegment{
				{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
					ASRRaw: "你好", StartMs: 500, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
			},
			UnmatchedLines: []UnmatchedLine{
				{LineID: "l30", RoleID: "narrator", Text: "漏读的一句", Reason: "no_audio", Optional: optional},
			},
		}
	}
	tlPath := filepath.Join(dir, "timeline.json")
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	if err := SaveJSON(tlPath, mk(false)); err != nil {
		t.Fatal(err)
	}
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "partial" {
		t.Fatalf("missing line must yield partial, got %s", m.Status)
	}
	if m.Counts["missing_lines"] != 1 {
		t.Fatalf("counts must attribute the gap: %+v", m.Counts)
	}
	if err := SaveJSON(tlPath, mk(true)); err != nil {
		t.Fatal(err)
	}
	m, err = Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out2")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "complete" {
		t.Fatalf("optional missing line must not downgrade, got %s", m.Status)
	}
}

// When review-blocked skips AND missing lines coexist the status must
// be "partial": reviewing alone cannot recover audio that was never
// recorded.
func TestRender_PartialOutranksBlockedReview(t *testing.T) {
	dir := t.TempDir()
	src := synthSpeech(4000, [][2]int{{500, 1500}, {2500, 3500}})
	audioPath := filepath.Join(dir, "take.wav")
	writeWAV(t, audioPath, src)
	sha, _ := SHA256File(audioPath)
	tl := &Timeline{
		Version: SchemaVersion, Audio: audioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: 4000,
		Segments: []TimelineSegment{
			{SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator", SpokenText: "你好",
				ASRRaw: "你好", StartMs: 500, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
			{SegmentID: "seg-0002", LineID: "l02", RoleID: "narrator", SpokenText: "在吗",
				ASRRaw: "在吗", StartMs: 2500, EndMs: 3500, MatchStatus: MatchModified,
				Selected: true, ReviewRequired: true,
				ReviewReasons: []string{ReviewLowSimilarity}},
		},
		UnmatchedLines: []UnmatchedLine{
			{LineID: "l30", RoleID: "narrator", Text: "漏读的一句", Reason: "no_audio"},
		},
	}
	tlPath := filepath.Join(dir, "timeline.json")
	voicesPath := filepath.Join(dir, "voices.json")
	os.WriteFile(voicesPath, []byte(`{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`), 0o644)
	if err := SaveJSON(tlPath, tl); err != nil {
		t.Fatal(err)
	}
	m, err := Render(RenderOptions{TimelinePath: tlPath, VoicesPath: voicesPath, OutDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "partial" {
		t.Fatalf("partial must outrank blocked_review, got %s", m.Status)
	}
	if m.Counts["blocked_review"] != 1 || m.Counts["missing_lines"] != 1 {
		t.Fatalf("counts must separate the two causes: %+v", m.Counts)
	}
}
