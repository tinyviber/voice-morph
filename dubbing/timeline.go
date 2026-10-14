package dubbing

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Match statuses for TimelineSegment.MatchStatus.
const (
	// MatchExact means the read text equals the script line (after
	// normalization it is identical, or nearly so).
	MatchExact = "exact"
	// MatchModified means the segment is bound to a line but the actual
	// wording differs: dropped/added words or a paraphrase.
	MatchModified = "modified"
	// MatchRepeated means the segment is one of several candidate takes
	// of the same line; only the selected take is rendered.
	MatchRepeated = "repeated"
	// MatchUnmatched means the audio matched no script line (ad-libs,
	// false starts, off-script chatter). It is kept for review and never
	// silently assigned a role.
	MatchUnmatched = "unmatched"
	// MatchManual means a human bound an unmatched segment to a role in
	// review (an improvised line, or a line_id fill-in). It renders like
	// a normal bound segment; Edited is always set on it.
	MatchManual = "manual"
)

// Review reasons recorded in TimelineSegment.ReviewReasons.
const (
	// ReviewLowSimilarity: text similarity below the auto-trust floor.
	ReviewLowSimilarity = "low_similarity"
	// ReviewEstimatedBoundary: the time boundary between two lines inside
	// one speech segment was interpolated, not measured.
	ReviewEstimatedBoundary = "estimated_boundary"
	// ReviewAmbiguousTakes: candidate takes score too close to pick
	// safely without a human.
	ReviewAmbiguousTakes = "ambiguous_takes"
	// ReviewUnmatchedAudio: audio that matched no script line.
	ReviewUnmatchedAudio = "unmatched_audio"
	// ReviewLowASRConfidence: the backend reported low recognition
	// confidence for this span.
	ReviewLowASRConfidence = "low_asr_confidence"
	// ReviewNeedsRole: audio is bound but no role could be assigned.
	ReviewNeedsRole = "needs_role"
)

// TimelineSegment is one span of real audio bound (or not) to a script
// line. When one ASR segment covers several lines it is split into
// consecutive TimelineSegments sharing ASRSegmentID; when one line spans
// several speech segments it produces several TimelineSegments sharing
// LineID (adjacent parts, TakeIndex == 0).
type TimelineSegment struct {
	SegmentID    string   `json:"segment_id"`
	ASRSegmentID string   `json:"asr_segment_id"`
	LineID       string   `json:"line_id,omitempty"`
	RoleID       string   `json:"role_id,omitempty"`
	ScriptText   string   `json:"script_text,omitempty"`
	ASRRaw       string   `json:"asr_raw"`
	SpokenText   string   `json:"spoken_text"`
	StartMs      int      `json:"start_ms"`
	EndMs        int      `json:"end_ms"`
	MatchStatus  string   `json:"match_status"`
	Similarity   float64  `json:"similarity"` // 0..1 text agreement score
	ReviewRequired bool   `json:"review_required,omitempty"`
	ReviewReasons  []string `json:"review_reasons,omitempty"`
	// TakeIndex numbers candidate takes of the same line (1..N); 0 means
	// this is the only take. Selected marks the take that renders.
	TakeIndex int  `json:"take_index,omitempty"`
	Selected  bool `json:"selected"`
	// PartIndex numbers consecutive parts of one line that was split
	// across speech segments (TakeIndex stays 0 — parts are one reading,
	// not competing takes). 0 means the line rendered as a single span.
	PartIndex int `json:"part_index,omitempty"`
	// Boundary is "measured" (real segment edge), "word" (snapped to a
	// word timestamp) or "estimated" (interpolated inside a segment).
	Boundary string `json:"boundary,omitempty"`
	// Edited marks text/role/bounds touched by a human during review; it
	// is sticky across re-renders.
	Edited bool `json:"edited,omitempty"`
}

// UnmatchedLine is a script line with no detected audio.
type UnmatchedLine struct {
	LineID  string `json:"line_id"`
	SceneID string `json:"scene_id,omitempty"`
	RoleID  string `json:"role_id"`
	Text    string `json:"text"`
	Reason  string `json:"reason"` // e.g. "no_audio"
}

// Timeline is the versioned reconciliation document (timeline.json). It
// is both the output of reconcile and the input of render — the user's
// reviewed edits live here, so re-rendering never re-runs ASR.
type Timeline struct {
	Version     int    `json:"version"`
	Audio       string `json:"audio"`
	AudioSHA256 string `json:"audio_sha256,omitempty"`
	SampleRate  int    `json:"sample_rate"`
	DurationMs  int    `json:"duration_ms"`
	Script      string `json:"script,omitempty"`    // source script path
	ASRFile     string `json:"asr_file,omitempty"`  // source asr-raw path
	Segments    []TimelineSegment `json:"segments"`
	UnmatchedLines   []UnmatchedLine `json:"unmatched_lines,omitempty"`
	ReviewCount int `json:"review_count"`
	Warnings    []string `json:"warnings,omitempty"`
}

// ParseTimeline decodes and validates a timeline document.
func ParseTimeline(data []byte) (*Timeline, error) {
	var t Timeline
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("timeline: invalid JSON: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// LoadTimeline reads and validates a timeline file.
func LoadTimeline(path string) (*Timeline, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read timeline %s: %w", path, err)
	}
	return ParseTimeline(b)
}

// Validate checks structural invariants of the timeline.
func (t *Timeline) Validate() error {
	if t.Version < 1 || t.Version > SchemaVersion {
		return fmt.Errorf("timeline: unsupported version %d (this build reads 1..%d)", t.Version, SchemaVersion)
	}
	if t.Audio == "" {
		return fmt.Errorf("timeline: missing audio path")
	}
	if t.SampleRate <= 0 {
		return fmt.Errorf("timeline: bad sample_rate %d", t.SampleRate)
	}
	ids := map[string]bool{}
	for i, s := range t.Segments {
		if s.SegmentID == "" {
			return fmt.Errorf("timeline: segment %d has empty segment_id", i)
		}
		if ids[s.SegmentID] {
			return fmt.Errorf("timeline: duplicate segment_id %q", s.SegmentID)
		}
		ids[s.SegmentID] = true
		if s.StartMs < 0 || s.EndMs <= s.StartMs {
			return fmt.Errorf("timeline: segment %s has invalid range %d..%d", s.SegmentID, s.StartMs, s.EndMs)
		}
		switch s.MatchStatus {
		case MatchExact, MatchModified, MatchRepeated, MatchUnmatched, MatchManual:
		default:
			return fmt.Errorf("timeline: segment %s has unknown match_status %q", s.SegmentID, s.MatchStatus)
		}
		if s.MatchStatus != MatchUnmatched && s.MatchStatus != MatchManual && s.LineID == "" {
			return fmt.Errorf("timeline: segment %s bound to empty line_id", s.SegmentID)
		}
	}
	return nil
}

// SaveJSON writes the document with stable indentation to path.
func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o644)
}

// SelectedSegments returns the render-ready segments in timeline order:
// selected, bound to a line, with a role assigned.
func (t *Timeline) SelectedSegments() []TimelineSegment {
	var out []TimelineSegment
	for _, s := range t.Segments {
		if s.Selected && s.LineID != "" {
			out = append(out, s)
		}
	}
	return out
}

// PendingReview returns segments still flagged for review.
func (t *Timeline) PendingReview() []TimelineSegment {
	var out []TimelineSegment
	for _, s := range t.Segments {
		if s.ReviewRequired {
			out = append(out, s)
		}
	}
	return out
}

// RefreshReviewCount recomputes ReviewCount from the segment flags plus
// unmatched lines.
func (t *Timeline) RefreshReviewCount() {
	n := len(t.UnmatchedLines)
	for _, s := range t.Segments {
		if s.ReviewRequired {
			n++
		}
	}
	t.ReviewCount = n
}

// ReorderSortsByTime keeps segments in audio order (used after edits).
func (t *Timeline) ReorderSortsByTime() {
	sort.SliceStable(t.Segments, func(i, j int) bool {
		if t.Segments[i].StartMs != t.Segments[j].StartMs {
			return t.Segments[i].StartMs < t.Segments[j].StartMs
		}
		return t.Segments[i].SegmentID < t.Segments[j].SegmentID
	})
}
