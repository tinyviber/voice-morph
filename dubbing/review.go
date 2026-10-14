package dubbing

import (
	"fmt"
	"sort"
)

// MergeReviewed folds human edits from a reviewed timeline back into a
// fresh reconcile draft, keyed by (line_id, asr_ref, start within
// 2s). Re-running ASR or reconcile never forces the reviewer to redo
// work: role/spoken/bounds/selection carry over; draft review flags
// are recomputed and cleared only where the human actually confirmed
// (Reviewed) — an edit alone never discharges review.
// Edits are refused outright when the two files describe different
// audio — a stale reviewed file must never graft onto another take.
func MergeReviewed(draft, reviewed *Timeline) (*Timeline, error) {
	if draft == nil || reviewed == nil {
		return draft, nil
	}
	if draft.AudioSHA256 != "" && reviewed.AudioSHA256 != "" &&
		draft.AudioSHA256 != reviewed.AudioSHA256 {
		return nil, fmt.Errorf("timeline-reviewed.json 指向不同录音（audio_sha256 不符），拒绝合并旧审核")
	}
	type key struct {
		line, ref string
		bucket    int
	}
	byLine := map[string][]TimelineSegment{}
	byKey := map[key][]TimelineSegment{}
	for _, s := range reviewed.Segments {
		byLine[s.LineID] = append(byLine[s.LineID], s)
		k := key{s.LineID, s.ASRSegmentID, s.StartMs / 2000}
		byKey[k] = append(byKey[k], s)
	}
	for i, s := range draft.Segments {
		k := key{s.LineID, s.ASRSegmentID, s.StartMs / 2000}
		var src *TimelineSegment
		if cands := byKey[k]; len(cands) > 0 {
			src = &cands[0]
		} else {
			// Fallback: same line, nearest asr_ref in time.
			for _, c := range byLine[s.LineID] {
				if c.ASRSegmentID == s.ASRSegmentID || (c.ASRSegmentID == "" && absInt(c.StartMs-s.StartMs) < 2000) {
					src = &c
					break
				}
			}
		}
		if src == nil || !(src.Edited || src.Reviewed) {
			continue
		}
		s.RoleID = src.RoleID
		s.SpokenText = src.SpokenText
		if src.StartMs != s.StartMs || src.EndMs != s.EndMs {
			s.StartMs, s.EndMs = src.StartMs, src.EndMs
			s.Boundary = "manual"
		}
		s.Selected = src.Selected
		if src.MatchStatus == MatchManual {
			s.MatchStatus = MatchManual
		}
		s.Edited = s.Edited || src.Edited
		if src.Reviewed {
			s.Reviewed = true
			s.ReviewRequired = false
			s.ReviewReasons = nil
		}
		draft.Segments[i] = s
	}
	// Manual binds by asr_ref: a reviewer may bind an unmatched audio
	// span to an existing line. The draft seg still has LineID="" so
	// the (line,ref) key above can't see it — match on the ASR ref.
	byRef := map[string]int{}
	for i := range reviewed.Segments {
		if reviewed.Segments[i].ASRSegmentID != "" {
			byRef[reviewed.Segments[i].ASRSegmentID] = i
		}
	}
	boundLines := map[string]bool{}
	for i, s := range draft.Segments {
		j, ok := byRef[s.ASRSegmentID]
		if !ok || s.LineID != "" || s.ASRSegmentID == "" {
			continue
		}
		src := reviewed.Segments[j]
		if src.MatchStatus != MatchManual || src.LineID == "" {
			continue
		}
		s.LineID = src.LineID
		s.ScriptText = src.ScriptText
		s.RoleID = src.RoleID
		s.SpokenText = src.SpokenText
		s.MatchStatus = MatchManual
		s.Selected = src.Selected || s.Selected
		s.Edited = true
		if src.Reviewed {
			s.Reviewed = true
			s.ReviewRequired = false
			s.ReviewReasons = nil
		}
		s.Discarded = src.Discarded
		draft.Segments[i] = s
		boundLines[s.LineID] = true
	}
	// Carry Discarded verdicts for still-unmatched segs too.
	for i, s := range draft.Segments {
		if s.MatchStatus != MatchUnmatched || s.ASRSegmentID == "" {
			continue
		}
		if j, ok := byRef[s.ASRSegmentID]; ok && reviewed.Segments[j].Discarded {
			draft.Segments[i].Discarded = true
		}
	}
	// UnmatchedLines: a bound line is no longer missing; optional
	// markers from the reviewed file survive the re-run.
	if len(draft.UnmatchedLines) > 0 {
		optional := map[string]bool{}
		for _, u := range reviewed.UnmatchedLines {
			optional[u.LineID] = optional[u.LineID] || u.Optional
		}
		var kept []UnmatchedLine
		for _, u := range draft.UnmatchedLines {
			if boundLines[u.LineID] {
				continue
			}
			u.Optional = u.Optional || optional[u.LineID]
			kept = append(kept, u)
		}
		draft.UnmatchedLines = kept
	}
	// Take selection by line: if the reviewed file picked a different
	// take, honor it.
	for lid, group := range takeGroups(reviewed) {
		if len(group) < 2 {
			continue
		}
		for _, rs := range group {
			if !rs.Selected {
				continue
			}
			for i, ds := range draft.Segments {
				if ds.LineID == lid && ds.ASRSegmentID == rs.ASRSegmentID {
					draft.Segments[i].Selected = true
				} else if ds.LineID == lid && ds.ASRSegmentID != rs.ASRSegmentID && draft.Segments[i].TakeIndex > 0 {
					draft.Segments[i].Selected = false
				}
			}
		}
	}
	draft.RefreshReviewCount()
	draft.ReorderSortsByTime()
	return draft, nil
}

func takeGroups(t *Timeline) map[string][]TimelineSegment {
	g := map[string][]TimelineSegment{}
	for _, s := range t.Segments {
		if s.LineID != "" && s.TakeIndex > 0 {
			g[s.LineID] = append(g[s.LineID], s)
		}
	}
	for _, v := range g {
		sort.SliceStable(v, func(i, j int) bool { return v[i].StartMs < v[j].StartMs })
	}
	return g
}

