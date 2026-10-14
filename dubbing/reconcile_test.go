package dubbing

import (
	"testing"
	"fmt"
	"strings"
)

func scriptOf(lines ...[3]string) *Script {
	s := &Script{Version: 1}
	for i, l := range lines {
		s.Lines = append(s.Lines, ScriptLine{
			LineID:  "l" + itoa(i+1),
			SceneID: l[0],
			RoleID:  l[1],
			Text:    l[2],
		})
	}
	if err := s.Validate(); err != nil {
		panic(err)
	}
	return s
}

func itoa(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func asrOf(segs ...[3]any) *ASRDoc {
	d := &ASRDoc{Version: 1, Audio: "take.wav", SampleRate: 48000}
	for i, s := range segs {
		d.Segments = append(d.Segments, ASRSegment{
			SegmentID: "seg-" + itoa(i+1),
			StartMs:   s[0].(int),
			EndMs:     s[1].(int),
			Text:      s[2].(string),
		})
		if d.DurationMs < s[1].(int) {
			d.DurationMs = s[1].(int)
		}
	}
	if err := d.Validate(); err != nil {
		panic(err)
	}
	return d
}

func bound(t *testing.T, tl *Timeline) []TimelineSegment {
	t.Helper()
	return tl.Segments
}

func findByLine(tl *Timeline, lineID string) []TimelineSegment {
	var out []TimelineSegment
	for _, s := range tl.Segments {
		if s.LineID == lineID {
			out = append(out, s)
		}
	}
	return out
}

// 完全一致
func TestReconcileExact(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "narrator", "他以为自己已经掌握了秘诀。"},
		[3]string{"s01", "character-a", "等等，我们明天再去训练。"},
	)
	asr := asrOf(
		[3]any{0, 3000, "他以为自己已经掌握了秘诀"},
		[3]any{4000, 8000, "等等我们明天再去训练"},
	)
	tl, err := Reconcile(sc, asr, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	segs := bound(t, tl)
	if len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].LineID != "l01" || segs[1].LineID != "l02" {
		t.Fatalf("wrong binding: %+v", segs)
	}
	if segs[0].MatchStatus != MatchExact || segs[1].MatchStatus != MatchExact {
		t.Fatalf("want exact: %s %s", segs[0].MatchStatus, segs[1].MatchStatus)
	}
	if segs[0].RoleID != "narrator" || segs[1].RoleID != "character-a" {
		t.Fatalf("wrong roles: %+v", segs)
	}
	if segs[1].StartMs != 4000 || segs[1].EndMs != 8000 {
		t.Fatalf("times not from audio: %+v", segs[1])
	}
	if tl.ReviewCount != 0 || len(tl.UnmatchedLines) != 0 {
		t.Fatalf("unexpected review items: %d %+v", tl.ReviewCount, tl.UnmatchedLines)
	}
}

// 同义改写：等→等一下、明天→明天下午、句尾加"吧"
func TestReconcileModified(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "narrator", "他以为自己已经掌握了秘诀。"},
		[3]string{"s01", "character-a", "等等，我们明天再去训练。"},
	)
	asr := asrOf(
		[3]any{0, 3000, "他以为自己已经掌握了秘诀"},
		[3]any{4000, 8000, "等一下我们明天下午再去训练吧"},
	)
	tl, err := Reconcile(sc, asr, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	s := findByLine(tl, "l02")
	if len(s) != 1 {
		t.Fatalf("want 1 bound seg, got %+v", tl.Segments)
	}
	if s[0].MatchStatus != MatchModified {
		t.Fatalf("want modified, got %s", s[0].MatchStatus)
	}
	// 实际朗读内容保留，不被台本覆盖
	if s[0].SpokenText != "等一下我们明天下午再去训练吧" {
		t.Fatalf("spoken text overwritten by script: %q", s[0].SpokenText)
	}
	if s[0].RoleID != "character-a" {
		t.Fatalf("wrong role: %s", s[0].RoleID)
	}
}

// 漏字漏词
func TestReconcileDroppedWords(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "a", "明天下午我们去操场训练"},
	)
	asr := asrOf([3]any{0, 3000, "明天我们去训练"})
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	s := findByLine(tl, "l01")
	if len(s) != 1 || s[0].MatchStatus != MatchModified {
		t.Fatalf("want modified binding, got %+v", tl.Segments)
	}
	if s[0].Similarity >= 0.98 {
		t.Fatalf("similarity too high for dropped words: %v", s[0].Similarity)
	}
}

// 整句漏读
func TestReconcileSkippedLine(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "narrator", "开场白第一句"},
		[3]string{"s01", "a", "被漏掉的中间句"},
		[3]string{"s01", "b", "收尾的最后一句台词"},
	)
	asr := asrOf(
		[3]any{0, 2000, "开场白第一句"},
		[3]any{3000, 6000, "收尾的最后一句台词"},
	)
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	if len(tl.UnmatchedLines) != 1 || tl.UnmatchedLines[0].LineID != "l02" {
		t.Fatalf("skipped line not detected: %+v", tl.UnmatchedLines)
	}
	if tl.UnmatchedLines[0].Reason != "no_audio" {
		t.Fatalf("bad reason: %s", tl.UnmatchedLines[0].Reason)
	}
	if n := len(findByLine(tl, "l03")); n != 1 {
		t.Fatalf("l03 not bound: %+v", tl.Segments)
	}
}

// 整句重复朗读 → 保留全部候选 take，推荐一个，存疑时标记
func TestReconcileRepeatedTake(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "a", "我们今天去爬山"},
		[3]string{"s01", "b", "明天的安排再说"},
	)
	asr := asrOf(
		[3]any{0, 2000, "我们今天去"},       // 读了一半
		[3]any{2500, 5000, "我们今天去爬山"}, // 重读完整
		[3]any{6000, 9000, "明天的安排再说"},
	)
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	takes := findByLine(tl, "l01")
	if len(takes) != 2 {
		t.Fatalf("want 2 take candidates, got %+v", tl.Segments)
	}
	sel, other := 0, 0
	for _, tk := range takes {
		if tk.MatchStatus != MatchRepeated {
			t.Fatalf("take not marked repeated: %+v", tk)
		}
		if tk.Selected {
			sel++
			if tk.SpokenText != "我们今天去爬山" {
				t.Fatalf("wrong take selected: %+v", tk)
			}
		} else {
			other++
		}
	}
	if sel != 1 || other != 1 {
		t.Fatalf("take selection wrong: %+v", takes)
	}
	if n := len(findByLine(tl, "l02")); n != 1 {
		t.Fatalf("l02 not bound: %+v", tl.Segments)
	}
}

// 台词被停顿拆成两个语音段 → 同一 line_id 的两个片段
func TestReconcileLineSplitAcrossSegments(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "a", "我们今天下午去操场训练"},
		[3]string{"s01", "b", "好的没问题"},
	)
	asr := asrOf(
		[3]any{0, 2000, "我们今天下午"},
		[3]any{3000, 5000, "去操场训练"},
		[3]any{6000, 7500, "好的没问题"},
	)
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	parts := findByLine(tl, "l01")
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %+v", tl.Segments)
	}
	for _, p := range parts {
		if !p.Selected || p.TakeIndex != 0 {
			t.Fatalf("part wrongly flagged as take: %+v", p)
		}
	}
	if parts[0].PartIndex != 1 || parts[1].PartIndex != 2 {
		t.Fatalf("parts should be numbered 1..2 in time order: %+v", parts)
	}
	if parts[0].EndMs > parts[1].StartMs {
		t.Fatalf("parts overlap")
	}
}

// 两个角色在一个语音段内紧邻 → 边界拆分并标记估算
func TestReconcileTwoRolesOneSegment(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "a", "你明天来吗"},
		[3]string{"s01", "b", "我一定准时到"},
	)
	asr := asrOf(
		[3]any{0, 4000, "你明天来吗我一定准时到"}, // 一口气读了两句
	)
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	a := findByLine(tl, "l01")
	b := findByLine(tl, "l02")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want both lines bound, got %+v", tl.Segments)
	}
	if a[0].RoleID != "a" || b[0].RoleID != "b" {
		t.Fatalf("roles wrong: %+v %+v", a[0], b[0])
	}
	if a[0].EndMs <= a[0].StartMs || b[0].StartMs < a[0].EndMs {
		t.Fatalf("boundary wrong: %+v %+v", a[0], b[0])
	}
	if b[0].Boundary != "estimated" && b[0].Boundary != "word" {
		t.Fatalf("interior boundary should be estimated/word: %s", b[0].Boundary)
	}
	if !b[0].ReviewRequired {
		t.Fatalf("interior boundary should be flagged for review")
	}
	if a[0].ASRSegmentID != b[0].ASRSegmentID {
		t.Fatalf("should share asr segment: %s %s", a[0].ASRSegmentID, b[0].ASRSegmentID)
	}
}

// 不匹配内容：编排外的口播保留为 unmatched 并标记审核，不猜角色
func TestReconcileUnmatchedAudio(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "a", "今天天气不错"},
		[3]string{"s01", "b", "我们出发吧"},
	)
	asr := asrOf(
		[3]any{0, 1500, "今天天气不错"},
		[3]any{2000, 3000, "哎呀我刚才走神了"}, // 题外话
		[3]any{4000, 5500, "我们出发吧"},
	)
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	var un []TimelineSegment
	for _, s := range tl.Segments {
		if s.MatchStatus == MatchUnmatched {
			un = append(un, s)
		}
	}
	if len(un) != 1 {
		t.Fatalf("want 1 unmatched, got %+v", tl.Segments)
	}
	if un[0].RoleID != "" {
		t.Fatalf("unmatched seg must not get a guessed role: %+v", un[0])
	}
	if un[0].Selected {
		t.Fatalf("unmatched seg must not render")
	}
	if !un[0].ReviewRequired {
		t.Fatalf("unmatched seg must require review")
	}
	// 顺序正确：unmatched 夹在两条绑定之间
	if tl.Segments[1].MatchStatus != MatchUnmatched {
		t.Fatalf("order broken: %+v", tl.Segments)
	}
}

// ASR 原始证据保留：asr_raw 不被台本覆盖
func TestReconcilePreservesASREvidence(t *testing.T) {
	sc := scriptOf([3]string{"s01", "a", "我们明天去训练"})
	asr := asrOf([3]any{0, 3000, "我们后天去训练"})
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	s := tl.Segments[0]
	if s.ASRRaw != "我们后天去训练" || s.SpokenText != "我们后天去训练" {
		t.Fatalf("asr evidence lost: %+v", s)
	}
	if s.ScriptText != "我们明天去训练" {
		t.Fatalf("script text lost: %+v", s)
	}
	if s.MatchStatus != MatchModified {
		t.Fatalf("substitution should be modified: %s", s.MatchStatus)
	}
}

// 多句连续朗读在一个段里（同一角色）：按行切开
func TestReconcileMultiLineOneSegment(t *testing.T) {
	sc := scriptOf(
		[3]string{"s01", "a", "第一句台词"},
		[3]string{"s01", "a", "第二句台词"},
		[3]string{"s01", "a", "第三句台词"},
	)
	asr := asrOf([3]any{0, 6000, "第一句台词第二句台词第三句台词"})
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	for _, id := range []string{"l01", "l02", "l03"} {
		if n := len(findByLine(tl, id)); n != 1 {
			t.Fatalf("line %s not bound: %+v", id, tl.Segments)
		}
	}
	// 时间单调不重叠
	prev := -1
	for _, s := range tl.Segments {
		if s.StartMs < prev {
			t.Fatalf("segments overlap: %+v", tl.Segments)
		}
		prev = s.StartMs
	}
}

// 口误后重录：错误开头 + 完整重读
func TestReconcileFalseStartThenRetake(t *testing.T) {
	sc := scriptOf([3]string{"s01", "a", "我们明天下午去训练"})
	asr := asrOf(
		[3]any{0, 1000, "我们明"},           // 口误中止
		[3]any{1500, 5000, "我们明天下午去训练"}, // 重读
	)
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	takes := findByLine(tl, "l01")
	if len(takes) != 2 {
		t.Fatalf("want false-start + retake as 2 candidates, got %+v", tl.Segments)
	}
	sel := 0
	for _, tk := range takes {
		if tk.Selected {
			sel++
			if tk.EndMs != 5000 {
				t.Fatalf("should select full retake: %+v", tk)
			}
		}
	}
	if sel != 1 {
		t.Fatalf("no take selected")
	}
}

// 台本之外的大段口播 + 低置信度
func TestReconcileLowConfidenceFlag(t *testing.T) {
	sc := scriptOf([3]string{"s01", "a", "今天天气不错"})
	d := asrOf([3]any{0, 1500, "今田天气不错"})
	d.Segments[0].Confidence = 0.3
	tl, _ := Reconcile(sc, d, DefaultReconcileOptions())
	s := tl.Segments[0]
	found := false
	for _, r := range s.ReviewReasons {
		if r == ReviewLowASRConfidence {
			found = true
		}
	}
	if !found {
		t.Fatalf("low asr confidence not flagged: %+v", s.ReviewReasons)
	}
}

// 保存/加载 round-trip
func TestTimelineRoundTrip(t *testing.T) {
	sc := scriptOf([3]string{"s01", "a", "我们明天去训练"})
	asr := asrOf([3]any{0, 3000, "我们明天去训练"})
	tl, _ := Reconcile(sc, asr, DefaultReconcileOptions())
	if err := tl.Validate(); err != nil {
		t.Fatalf("emitted timeline invalid: %v", err)
	}
}

func TestSplitFusedSegments(t *testing.T) {
	words := func(start, step int, texts ...string) []ASRWord {
		out := make([]ASRWord, len(texts))
		for i, x := range texts {
			out[i] = ASRWord{Text: x, StartMs: start + i*step, EndMs: start + i*step + step}
		}
		return out
	}
	one := ASRSegment{SegmentID: "s1", StartMs: 0, EndMs: 6000,
		Text: "你好。世界。", Coarse: true,
		Words: words(100, 500, "你", "好", "。", "世", "界", "。")}
	got := SplitFusedSegments([]ASRSegment{one})
	if len(got) != 2 {
		t.Fatalf("want 2 splits, got %d", len(got))
	}
	if got[0].Text != "你好。" || got[1].Text != "世界。" {
		t.Fatalf("split text wrong: %q / %q", got[0].Text, got[1].Text)
	}
	if got[0].Coarse || got[1].Coarse {
		t.Fatal("word-derived bounds must not stay coarse")
	}
	if got[0].StartMs != 100 || got[0].EndMs != 1600 || got[1].StartMs != 1600 {
		t.Fatalf("bounds wrong: %+v / %+v", got[0], got[1])
	}
	// Text-only (no words) splits proportionally and stays coarse.
	two := ASRSegment{SegmentID: "s2", StartMs: 0, EndMs: 4000,
		Text: "第一句。第二句。", Coarse: true}
	got = SplitFusedSegments([]ASRSegment{two})
	if len(got) != 2 || !got[0].Coarse {
		t.Fatalf("text-only split: %d segs coarse=%v", len(got), got[0].Coarse)
	}
	// Single sentence untouched.
	three := ASRSegment{SegmentID: "s3", StartMs: 0, EndMs: 2000, Text: "没有标点"}
	got = SplitFusedSegments([]ASRSegment{three})
	if len(got) != 1 || got[0].SegmentID != "s3" {
		t.Fatalf("single sentence should pass through: %+v", got)
	}
}

// Long-recording regression: an 8-minute take where the ASR fused
// every utterance into ONE segment (no VAD) must still bind all
// ~60 lines — the per-block line cap must not silently drop lines.
func TestReconcile_LongRecordingFused(t *testing.T) {
	const nLines = 60
	var lines []ScriptLine
	var full strings.Builder
	var words []ASRWord
	pos := 0 // ms
	for i := 0; i < nLines; i++ {
		role := "narrator"
		if i%3 == 1 {
			role = "character-a"
		} else if i%3 == 2 {
			role = "character-b"
		}
		text := fmt.Sprintf("这是第%d句对白内容", i)
		lines = append(lines, ScriptLine{LineID: fmt.Sprintf("l%02d", i+1),
			SceneID: "s1", RoleID: role, Text: text})
		// utterance: ~7s speech, ~1s gap; each char a 350ms word
		runes := []rune(text)
		for j, r := range runes {
			words = append(words, ASRWord{Text: string(r),
				StartMs: pos + j*350, EndMs: pos + j*350 + 330})
		}
		end := pos + len(runes)*350
		words = append(words, ASRWord{Text: "。", StartMs: end, EndMs: end + 100})
		full.WriteString(text + "。")
		pos = end + 1100
	}
	sc := &Script{Version: 1, Lines: lines}
	doc := &ASRDoc{Version: 1, Audio: "long.wav", SampleRate: 48000,
		DurationMs: pos,
		Segments: []ASRSegment{{SegmentID: "fused-1", StartMs: 0,
			EndMs: pos, Text: full.String(), Words: words, Coarse: true}}}
	tl, err := Reconcile(sc, doc, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]bool{}
	for _, s := range tl.Segments {
		if s.LineID != "" && s.Selected {
			bound[s.LineID] = true
		}
	}
	if len(bound) != nLines {
		t.Fatalf("bound %d/%d lines; unmatched_lines=%d", len(bound), nLines, len(tl.UnmatchedLines))
	}
	if len(tl.UnmatchedLines) != 0 {
		t.Fatalf("%d recognized lines wrongly unmatched", len(tl.UnmatchedLines))
	}
}

// No-punctuation fused segment WITH word timestamps: the ASR dropped
// every 。 but the ~1s silence between utterances must still cut it
// into sentence pieces at the word-timestamp pauses.
func TestReconcile_FusedNoPunctPauses(t *testing.T) {
	const nLines = 60
	var lines []ScriptLine
	var full strings.Builder
	var words []ASRWord
	pos := 0
	for i := 0; i < nLines; i++ {
		text := fmt.Sprintf("这是第%d句对白内容", i)
		lines = append(lines, ScriptLine{LineID: fmt.Sprintf("l%02d", i+1),
			SceneID: "s1", RoleID: "narrator", Text: text})
		runes := []rune(text)
		for j, r := range runes {
			words = append(words, ASRWord{Text: string(r),
				StartMs: pos + j*350, EndMs: pos + j*350 + 330})
		}
		full.WriteString(text) // NO punctuation at all
		pos = pos + len(runes)*350 + 1100 // 1.1s inter-sentence silence
	}
	doc := &ASRDoc{Version: 1, Audio: "long.wav", SampleRate: 48000,
		DurationMs: pos,
		Segments: []ASRSegment{{SegmentID: "fused-1", StartMs: 0,
			EndMs: pos, Text: full.String(), Words: words, Coarse: true}}}
	spl := SplitFusedSegments(doc.Segments)
	if len(spl) != nLines {
		t.Fatalf("pause-split produced %d segments, want %d", len(spl), nLines)
	}
	tl, err := Reconcile(&Script{Version: 1, Lines: lines}, doc, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]bool{}
	for _, s := range tl.Segments {
		if s.LineID != "" && s.Selected {
			bound[s.LineID] = true
		}
	}
	if len(bound) != nLines || len(tl.UnmatchedLines) != 0 {
		t.Fatalf("bound %d/%d unmatched_lines=%d", len(bound), nLines, len(tl.UnmatchedLines))
	}
}

// Fully boundless fused segment — no punctuation AND no word
// timestamps. Splitting is impossible; instead of reporting 56 lines
// as no_audio, the matcher must degrade: bind the lines inside the
// giant segment, mark every interior boundary estimated (review
// required), and warn on the timeline.
func TestReconcile_FusedBoundless(t *testing.T) {
	const nLines = 60
	var lines []ScriptLine
	var full strings.Builder
	for i := 0; i < nLines; i++ {
		text := fmt.Sprintf("这是第%d句对白内容", i)
		lines = append(lines, ScriptLine{LineID: fmt.Sprintf("l%02d", i+1),
			SceneID: "s1", RoleID: "narrator", Text: text})
		full.WriteString(text)
	}
	doc := &ASRDoc{Version: 1, Audio: "long.wav", SampleRate: 48000,
		DurationMs: 480000,
		Segments: []ASRSegment{{SegmentID: "fused-1", StartMs: 0,
			EndMs: 480000, Text: full.String(), Coarse: true}}}
	spl := SplitFusedSegments(doc.Segments)
	if len(spl) != 1 {
		t.Fatalf("boundless seg must survive splitting, got %d", len(spl))
	}
	tl, err := Reconcile(&Script{Version: 1, Lines: lines}, doc, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]bool{}
	estimated := 0
	for _, s := range tl.Segments {
		if s.LineID != "" && s.Selected {
			bound[s.LineID] = true
		}
		for _, r := range s.ReviewReasons {
			if r == ReviewEstimatedBoundary {
				estimated++
			}
		}
	}
	if len(bound) != nLines {
		t.Fatalf("boundless fused seg dropped lines: bound %d/%d unmatched_lines=%d",
			len(bound), nLines, len(tl.UnmatchedLines))
	}
	if len(tl.Warnings) == 0 {
		t.Fatal("boundless degradation must warn on the timeline")
	}
	if estimated == 0 {
		t.Fatal("boundless pieces must carry estimated_boundary")
	}
}

// Fast continuous read: word ts exist but every inter-sentence pause
// is 250ms — below PauseGapMs. The splitter can't cut; the matcher
// must degrade on residual size (not on missing words) and bind all
// 60 lines with word-mapped boundaries instead of reporting no_audio.
func TestReconcile_FusedShortPauses(t *testing.T) {
	const nLines = 60
	var lines []ScriptLine
	var full strings.Builder
	var words []ASRWord
	pos := 0
	for i := 0; i < nLines; i++ {
		text := fmt.Sprintf("这是第%d句对白内容", i)
		lines = append(lines, ScriptLine{LineID: fmt.Sprintf("l%02d", i+1),
			SceneID: "s1", RoleID: "narrator", Text: text})
		runes := []rune(text)
		for j, r := range runes {
			words = append(words, ASRWord{Text: string(r),
				StartMs: pos + j*350, EndMs: pos + j*350 + 330})
		}
		full.WriteString(text)
		pos = pos + len(runes)*350 + 250 // fast read: 250ms < PauseGapMs
	}
	doc := &ASRDoc{Version: 1, Audio: "long.wav", SampleRate: 48000,
		DurationMs: pos,
		Segments: []ASRSegment{{SegmentID: "fused-1", StartMs: 0,
			EndMs: pos, Text: full.String(), Words: words, Coarse: true}}}
	spl := SplitFusedSegments(doc.Segments)
	if len(spl) != 1 {
		t.Fatalf("short pauses must not split, got %d", len(spl))
	}
	tl, err := Reconcile(&Script{Version: 1, Lines: lines}, doc, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]bool{}
	for _, s := range tl.Segments {
		if s.LineID != "" && s.Selected {
			bound[s.LineID] = true
		}
	}
	if len(bound) != nLines || len(tl.UnmatchedLines) != 0 {
		t.Fatalf("fast-read fused seg dropped lines: bound %d/%d unmatched=%d",
			len(bound), nLines, len(tl.UnmatchedLines))
	}
	if len(tl.Warnings) == 0 {
		t.Fatal("degraded oversized seg must warn on the timeline")
	}
}

// Closest-to-production case: 100 lines alternating three roles, no
// punctuation, no pauses above the gap threshold — plus one skipped
// line, one retake and one paraphrase mixed in. Anchored pre-split
// must turn the fused input back into sentence-level pieces; binding
// must preserve line_id AND role, keep plausible monotonic timestamps,
// and still flag the retake and the paraphrase for review.
func TestReconcile_FusedMultiRoleRealistic(t *testing.T) {
	const nLines = 100
	roles := []string{"narrator", "character-a", "character-b"}
	var lines []ScriptLine
	var full strings.Builder
	var words []ASRWord
	pos := 0
	emit := func(text string, gapMs int) {
		for j, r := range []rune(text) {
			words = append(words, ASRWord{Text: string(r),
				StartMs: pos + j*350, EndMs: pos + j*350 + 330})
		}
		full.WriteString(text)
		pos += len([]rune(text))*350 + gapMs
	}
	for i := 0; i < nLines; i++ {
		text := fmt.Sprintf("第%d幕台词要说的内容", i)
		lines = append(lines, ScriptLine{LineID: fmt.Sprintf("l%03d", i+1),
			SceneID: "s1", RoleID: roles[i%3], Text: text})
		switch i {
		case 20:
			// skipped: never spoken
		case 40:
			emit("第40幕台词要设的内存", 200) // slip: retake follows
			emit(text, 200)
		case 60:
			emit("第60幕台词想表达的意思", 200) // paraphrase
		default:
			emit(text, 200)
		}
	}
	doc := &ASRDoc{Version: 1, Audio: "long.wav", SampleRate: 48000,
		DurationMs: pos,
		Segments: []ASRSegment{{SegmentID: "fused-1", StartMs: 0,
			EndMs: pos, Text: full.String(), Words: words, Coarse: true}}}
	tl, err := Reconcile(&Script{Version: 1, Lines: lines}, doc, DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	bound, lastEnd := map[string]*TimelineSegment{}, -1
	for i := range tl.Segments {
		s := &tl.Segments[i]
		if s.LineID == "" || !s.Selected {
			continue
		}
		if s.StartMs < lastEnd-2000 {
			t.Fatalf("timestamps not monotonic at %s (%d < %d)", s.SegmentID, s.StartMs, lastEnd)
		}
		lastEnd = s.EndMs
		bound[s.LineID] = s
	}
	// 99 of 100 lines bound (l021 intentionally unread).
	if len(bound) != nLines-1 {
		t.Fatalf("bound %d, want %d; unmatched=%v", len(bound), nLines-1, tl.UnmatchedLines)
	}
	skipFound := false
	for _, u := range tl.UnmatchedLines {
		if u.LineID == "l021" {
			skipFound = true
		}
	}
	if !skipFound {
		t.Fatal("skipped line l021 must appear in unmatched_lines")
	}
	// Role assignment preserved for a deep line.
	if bound["l100"] == nil || bound["l100"].RoleID != roles[99%3] {
		t.Fatalf("l100 role wrong: %+v", bound["l100"])
	}
	// Retake of l041 must surface as take candidates: the correct
	// reading selected, the slip retained unselected for review.
	var l041Sel, l041All []TimelineSegment
	for _, s := range tl.Segments {
		if s.LineID != "l041" {
			continue
		}
		l041All = append(l041All, s)
		if s.Selected {
			l041Sel = append(l041Sel, s)
		}
	}
	if len(l041All) < 2 {
		t.Fatalf("l041 retake not surfaced (segs=%+v)", l041All)
	}
	if len(l041Sel) != 1 || !strings.Contains(l041Sel[0].SpokenText, "要说的内容") {
		t.Fatalf("wrong take selected for l041: %+v", l041Sel)
	}
	// Paraphrase l061 must bind but carry a review flag (low_similarity
	// or ambiguous) — it must not look like a clean exact match.
	if s := bound["l061"]; s == nil {
		t.Fatal("paraphrased l061 unbound")
	} else if !s.ReviewRequired && s.MatchStatus == MatchExact {
		t.Fatalf("paraphrase looks too clean: %+v", s)
	}
}

// anchorSplit maps TEXT position → word index → timestamp. Under a
// variable speech rate a time-proportional cut snaps to the wrong
// word; the char-coverage map must land on the word actually holding
// that text position.
func TestAnchorSplit_VariableRate(t *testing.T) {
	var b strings.Builder
	var words []ASRWord
	pos := 0
	// 20 words × 2 norm chars: first 10 slow (500ms), last 10 fast
	// (100ms). Text midpoint is char 20, time midpoint would be ~3000 —
	// which sits inside word 6, NOT at the text boundary.
	texts := []string{"cc", "cc", "cc", "cc", "cc", "dd", "dd", "dd", "dd", "dd",
		"ee", "ee", "ee", "ee", "ee", "ff", "ff", "ff", "ff", "ff"}
	for j, w := range texts {
		dur := 500
		if j >= 10 {
			dur = 100
		}
		words = append(words, ASRWord{Text: w, StartMs: pos, EndMs: pos + dur})
		pos += dur
		b.WriteString(w)
	}
	norm, src := normRunes(b.String())
	seg := normSeg{seg: ASRSegment{SegmentID: "f", StartMs: 0, EndMs: pos,
		Text: b.String(), Words: words, Coarse: true},
		norm: norm, src: src, rs: []rune(b.String())}
	var lines []normLine
	for i, lt := range []string{"cccccccccc", "dddddddddd", "eeeeeeeeee", "ffffffffff"} {
		n, _ := normRunes(lt)
		lines = append(lines, normLine{line: ScriptLine{
			LineID: fmt.Sprintf("l%d", i), Text: lt}, norm: n})
	}
	pieces, n := anchorSplit(seg, lines, DefaultReconcileOptions())
	if n == 0 || len(pieces) < 4 {
		t.Fatalf("anchorSplit produced %d pieces", len(pieces))
	}
	// cuts at text pos 10/20/30 → word-mapped 2500/5000/5500.
	want := []int{2500, 5000, 5500}
	got := []int{pieces[1].seg.StartMs, pieces[2].seg.StartMs, pieces[3].seg.StartMs}
	for i := range want {
		if absInt(got[i]-want[i]) > 50 {
			t.Fatalf("cut %d: want ~%d (text-mapped), got %d", i, want[i], got[i])
		}
	}
}
