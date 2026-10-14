package dubbing

import (
	"fmt"
	"testing"
)

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

// Shared-ASR-ID collision: a fused ASR span split into several
// TimelineSegments must never let one sibling's verdict graft onto
// another. Three segments share asr-001; only the middle one is
// discarded — after a re-run (fresh segment_ids) the other two must
// keep their own state.
func TestMergeReviewed_SharedASRIDNoGraft(t *testing.T) {
	mkSegs := func(discardB bool) []TimelineSegment {
		segs := []TimelineSegment{
			{SegmentID: "seg-0001", ASRSegmentID: "asr-001", LineID: "l01",
				RoleID: "narrator", SpokenText: "正常对白甲", ASRRaw: "正常对白甲",
				StartMs: 0, EndMs: 1500, MatchStatus: MatchExact, Selected: true},
			{SegmentID: "seg-0002", ASRSegmentID: "asr-001", SpokenText: "口误碎片",
				ASRRaw: "口误碎片", StartMs: 1500, EndMs: 2600,
				MatchStatus: MatchUnmatched, ReviewRequired: true,
				ReviewReasons: []string{ReviewUnmatchedAudio}, Discarded: discardB},
			{SegmentID: "seg-0003", ASRSegmentID: "asr-001", LineID: "l02",
				RoleID: "narrator", SpokenText: "正常对白乙", ASRRaw: "正常对白乙",
				StartMs: 2600, EndMs: 4200, MatchStatus: MatchExact, Selected: true},
		}
		return segs
	}
	reviewed := &Timeline{Version: SchemaVersion, Audio: "t.wav", AudioSHA256: "sha",
		Segments: mkSegs(true)}
	// Re-run produced new segment_ids but identical asr refs/times/text.
	draft := &Timeline{Version: SchemaVersion, Audio: "t.wav", AudioSHA256: "sha",
		Segments: mkSegs(false)}
	for i := range draft.Segments {
		draft.Segments[i].SegmentID = fmt.Sprintf("new-%d", i)
	}
	m, err := MergeReviewed(draft, reviewed)
	if err != nil {
		t.Fatal(err)
	}
	// Middle seg keeps its discard; siblings must NOT inherit it.
	if !m.Segments[1].Discarded {
		t.Fatal("discard on middle seg lost")
	}
	if m.Segments[0].Discarded || m.Segments[2].Discarded {
		t.Fatalf("discard grafted onto siblings: %+v", m.Segments)
	}
	if !m.Segments[0].Selected || !m.Segments[2].Selected {
		t.Fatalf("siblings lost their state: %+v", m.Segments)
	}
}
