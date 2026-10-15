package dubbing

import (
	"fmt"
	"sort"
	"strings"
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
	// EVERY merge path resolves segments by STABLE IDENTITY — several
	// TimelineSegments can share one ASRSegmentID (a fused ASR span
	// split into lines, or two takes of the same line), so neither
	// edits nor take selection may key on asr_ref alone. Primary key
	// is segment_id; the fallback narrows asr_ref candidates by time
	// (±2s) AND normalized-text anchor, and refuses ambiguity — a lost
	// verdict stays review-pending, never misapplied.
	rIdx := newSegIndex(reviewed)
	dIdx := newSegIndex(draft)
	boundLines := map[string]bool{}
	for i, s := range draft.Segments {
		j, ok := rIdx.resolve(s, s.LineID != "")
		if !ok {
			continue
		}
		src := reviewed.Segments[j]
		if s.LineID == "" {
			// Unmatched draft seg: manual bind, or discarded carry.
			if src.MatchStatus == MatchManual && src.LineID != "" {
				s.LineID = src.LineID
				s.ScriptText = src.ScriptText
				s.RoleID = src.RoleID
				s.SpokenText = src.SpokenText
				s.MatchStatus = MatchManual
				s.Selected = src.Selected || s.Selected
				s.Edited = true
				s.Discarded = src.Discarded
				if src.Reviewed {
					s.Reviewed = true
					s.ReviewRequired = false
					s.ReviewReasons = nil
				}
				draft.Segments[i] = s
				boundLines[s.LineID] = true
			} else if src.Discarded && src.MatchStatus == MatchUnmatched {
				draft.Segments[i].Discarded = true
			} else if src.Edited || src.Reviewed {
				// Plain edits on unmatched audio (e.g. corrected
				// spoken text without binding).
				s.SpokenText = src.SpokenText
				s.Edited = s.Edited || src.Edited
				if src.Reviewed {
					s.Reviewed = true
					s.ReviewRequired = false
					s.ReviewReasons = nil
				}
				draft.Segments[i] = s
			}
			continue
		}
		if !(src.Edited || src.Reviewed) {
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
	// take, honor it — resolved by stable identity so two takes
	// sharing an ASR id can't both end up selected.
	for lid, group := range takeGroups(reviewed) {
		if len(group) < 2 {
			continue
		}
		for _, rs := range group {
			if !rs.Selected {
				continue
			}
			j, ok := dIdx.resolve(rs, true)
			if !ok || draft.Segments[j].TakeIndex == 0 {
				continue // ambiguous or resolved to a non-take — keep
				// the reconciler's own choice
			}
			// A take replaces the line's audio: the picked take wins, every
			// other segment bound to that line (other takes AND the part
			// segs) is deselected.
			for i := range draft.Segments {
				ds := &draft.Segments[i]
				if ds.LineID == lid {
					ds.Selected = i == j
				}
			}
		}
	}
	draft.RefreshReviewCount()
	draft.ReorderSortsByTime()
	return draft, nil
}

// segIndex supports stable-identity lookups across two timelines.
type segIndex struct {
	segs   []TimelineSegment
	byID   map[string]int
	byRef  map[string][]int
	byLine map[string][]int
}

func newSegIndex(t *Timeline) *segIndex {
	x := &segIndex{segs: t.Segments, byID: map[string]int{},
		byRef: map[string][]int{}, byLine: map[string][]int{}}
	for i, s := range t.Segments {
		x.byID[s.SegmentID] = i
		if s.ASRSegmentID != "" {
			x.byRef[s.ASRSegmentID] = append(x.byRef[s.ASRSegmentID], i)
		}
		if s.LineID != "" {
			x.byLine[s.LineID] = append(x.byLine[s.LineID], i)
		}
	}
	return x
}

// resolve finds the unique counterpart of s in the index. Identity:
// segment_id exact only when the ASR source, raw text, and time interval
// still agree; otherwise
// asr_ref candidates — or, for segments with no asr ref, same-line
// candidates — narrowed by close, overlapping time intervals and a
// normalized-text prefix anchor. The anchor compares ASRRaw (immutable
// as-recognized text) before falling back to SpokenText, so a heavy human
// rewrite can't break identity. Exactly one candidate wins; zero or several
// means "don't know" and the caller leaves the segment pending.
func (x *segIndex) resolve(s TimelineSegment, requireLine bool) (int, bool) {
	if j, ok := x.byID[s.SegmentID]; ok &&
		(!requireLine || x.segs[j].LineID == s.LineID) &&
		segmentIdentityEvidenceMatches(s, x.segs[j]) &&
		identityIntervalsOverlap(s, x.segs[j]) {
		return j, true
	}
	anchor := func(s TimelineSegment) string {
		if s.ASRRaw != "" {
			return normText(s.ASRRaw)
		}
		return normText(s.SpokenText)
	}
	var cand []int
	sn := anchor(s)
	add := func(j int) {
		r := x.segs[j]
		if requireLine && r.LineID != s.LineID {
			return
		}
		if !identityIntervalsOverlap(s, r) {
			return
		}
		rn := anchor(r)
		if rn == "" || sn == "" || strings.HasPrefix(rn, sn) ||
			strings.HasPrefix(sn, rn) {
			cand = append(cand, j)
		}
	}
	if s.ASRSegmentID != "" {
		for _, j := range x.byRef[s.ASRSegmentID] {
			add(j)
		}
	} else {
		for _, j := range x.byLine[s.LineID] {
			add(j)
		}
	}
	if len(cand) == 1 {
		return cand[0], true
	}
	return 0, false
}

func segmentIdentityEvidenceMatches(a, b TimelineSegment) bool {
	if a.ASRSegmentID != b.ASRSegmentID {
		return false
	}
	if a.ASRRaw != "" || b.ASRRaw != "" {
		return a.ASRRaw != "" && b.ASRRaw != "" && normText(a.ASRRaw) == normText(b.ASRRaw)
	}
	return normText(a.SpokenText) != "" && normText(a.SpokenText) == normText(b.SpokenText)
}

func identityIntervalsOverlap(a, b TimelineSegment) bool {
	if absInt(a.StartMs-b.StartMs) > 2000 || absInt(a.EndMs-b.EndMs) > 2000 {
		return false
	}
	start := a.StartMs
	if b.StartMs > start {
		start = b.StartMs
	}
	end := a.EndMs
	if b.EndMs < end {
		end = b.EndMs
	}
	overlap := end - start
	shorter := a.EndMs - a.StartMs
	if duration := b.EndMs - b.StartMs; duration < shorter {
		shorter = duration
	}
	return shorter > 0 && overlap*2 >= shorter
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
