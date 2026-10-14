package dubbing

import "testing"

func baseDraft() *Timeline {
	return &Timeline{
		Version: SchemaVersion, Audio: "take.wav", AudioSHA256: "sha",
		SampleRate: 48000, DurationMs: 6000,
		Segments: []TimelineSegment{
			{SegmentID: "seg-0001", ASRSegmentID: "a1", LineID: "l01", RoleID: "narrator",
				SpokenText: "你好", ASRRaw: "你好", StartMs: 0, EndMs: 1000,
				MatchStatus: MatchExact, Selected: true},
			{SegmentID: "seg-0002", ASRSegmentID: "a2", SpokenText: "对了还有一件事",
				ASRRaw: "对了还有一件事", StartMs: 1200, EndMs: 2600,
				MatchStatus: MatchUnmatched, ReviewRequired: true,
				ReviewReasons: []string{ReviewUnmatchedAudio}},
		},
		UnmatchedLines: []UnmatchedLine{
			{LineID: "l02", RoleID: "narrator", Text: "漏读的一句", Reason: "no_audio"},
			{LineID: "l03", RoleID: "narrator", Text: "允许省略", Reason: "no_audio"},
		},
	}
}

// A reviewer binds the unmatched audio to the missing line and marks
// the other line optional. Both verdicts must survive a re-reconcile:
// the bound seg becomes a manual render unit and l02 leaves
// unmatched_lines, while l03 keeps its optional flag.
func TestMergeReviewed_ManualBindAndOptional(t *testing.T) {
	reviewed := baseDraft()
	s := &reviewed.Segments[1]
	s.LineID = "l02"
	s.ScriptText = "漏读的一句"
	s.RoleID = "narrator"
	s.MatchStatus = MatchManual
	s.Selected = true
	s.Reviewed = true
	s.Edited = true
	reviewed.UnmatchedLines[0].Optional = false
	reviewed.UnmatchedLines[1].Optional = true

	m, err := MergeReviewed(baseDraft(), reviewed)
	if err != nil {
		t.Fatal(err)
	}
	var bound *TimelineSegment
	for i := range m.Segments {
		if m.Segments[i].ASRSegmentID == "a2" {
			bound = &m.Segments[i]
		}
	}
	if bound == nil || bound.MatchStatus != MatchManual || bound.LineID != "l02" ||
		bound.ScriptText != "漏读的一句" || bound.RoleID != "narrator" ||
		!bound.Selected || !bound.Reviewed || bound.ReviewRequired {
		t.Fatalf("manual bind not carried: %+v", bound)
	}
	for _, u := range m.UnmatchedLines {
		if u.LineID == "l02" {
			t.Fatalf("bound line still missing: %+v", m.UnmatchedLines)
		}
		if u.LineID == "l03" && !u.Optional {
			t.Fatalf("optional flag lost across merge: %+v", u)
		}
	}
	if len(m.UnmatchedLines) != 1 {
		t.Fatalf("want 1 unmatched line left, got %+v", m.UnmatchedLines)
	}
}

// A discarded verdict on unmatched audio must also carry across
// merges — otherwise the reviewer has to re-discard after every run.
func TestMergeReviewed_DiscardCarries(t *testing.T) {
	reviewed := baseDraft()
	reviewed.Segments[1].Discarded = true

	m, err := MergeReviewed(baseDraft(), reviewed)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Segments[1].Discarded {
		t.Fatal("discarded verdict lost across merge")
	}
}
