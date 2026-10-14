package dubbing

import (
	"strings"
	"encoding/json"
	"fmt"
	"os"
)

// ASRWord is one word/character unit with timestamps, when the ASR
// backend reports them. Absent means the backend has no word-level
// timing; reconciliation then estimates boundaries proportionally and
// marks them for review — timings are never fabricated.
type ASRWord struct {
	Text    string `json:"text"`
	StartMs int    `json:"start_ms"`
	EndMs   int    `json:"end_ms"`
}

// ASRSegment is one contiguous speech span as recognized.
type ASRSegment struct {
	SegmentID  string    `json:"segment_id"`
	StartMs    int       `json:"start_ms"`
	EndMs      int       `json:"end_ms"`
	Text       string    `json:"text"`
	Confidence float64   `json:"confidence,omitempty"`
	Words      []ASRWord `json:"words,omitempty"`
	// Coarse marks a segment whose bounds were derived from chunk
	// duration or token timestamps because the backend returned no
	// sentence-level timings — boundaries on it are estimates.
	Coarse     bool      `json:"coarse,omitempty"`
}

// ASRBackend describes what produced the transcript: which model, from
// where, and which capabilities it actually delivered this run. Consumers
// must consult WordTimestamps/Confidence before trusting those fields —
// installs differ across FunASR/ModelScope versions.
type ASRBackend struct {
	Name           string `json:"name"` // e.g. "sensevoice-small"
	Version        string `json:"version,omitempty"`
	ModelPath      string `json:"model_path,omitempty"`
	Language       string `json:"language,omitempty"`
	WordTimestamps bool   `json:"word_timestamps"`
	Confidence     bool   `json:"confidence,omitempty"`
	VAD            bool   `json:"vad,omitempty"`
}

// ASRDoc is the raw transcription record (asr-raw.json): the evidence the
// reconciliation works from, preserved verbatim for audit and re-runs.
type ASRDoc struct {
	Version     int          `json:"version"`
	Audio       string       `json:"audio"`
	AudioSHA256 string       `json:"audio_sha256,omitempty"`
	SampleRate  int          `json:"sample_rate"`
	DurationMs  int          `json:"duration_ms"`
	Backend     ASRBackend   `json:"backend"`
	Segments    []ASRSegment `json:"segments"`
	// Chunks records fallback chunking (file, time offset) so a merged
	// transcript stays auditable; empty when ASR ran in one pass.
	Chunks      []ASRChunk   `json:"chunks,omitempty"`
	ElapsedMs   int64        `json:"elapsed_ms,omitempty"`
}

// ASRChunk marks one Go-side audio chunk: which file was transcribed
// and where it sits on the master timeline.
type ASRChunk struct {
	File     string `json:"file"`
	OffsetMs int64  `json:"offset_ms"`
	EndMs    int64  `json:"end_ms"`
}

// ParseASRDoc decodes and validates an ASR document.
func ParseASRDoc(data []byte) (*ASRDoc, error) {
	var d ASRDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("asr-raw: invalid JSON: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// LoadASRDoc reads and validates an asr-raw file.
func LoadASRDoc(path string) (*ASRDoc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read asr-raw %s: %w", path, err)
	}
	return ParseASRDoc(b)
}

// Validate checks structural invariants of the ASR document.
func (d *ASRDoc) Validate() error {
	if d.Version < 1 || d.Version > SchemaVersion {
		return fmt.Errorf("asr-raw: unsupported version %d (this build reads 1..%d)", d.Version, SchemaVersion)
	}
	if d.Audio == "" {
		return fmt.Errorf("asr-raw: missing audio path")
	}
	if d.SampleRate <= 0 {
		return fmt.Errorf("asr-raw: bad sample_rate %d", d.SampleRate)
	}
	prev := -1
	for i, s := range d.Segments {
		if s.SegmentID == "" {
			return fmt.Errorf("asr-raw: segment %d has empty segment_id", i)
		}
		if s.StartMs < 0 || s.EndMs <= s.StartMs {
			return fmt.Errorf("asr-raw: segment %s has invalid range %d..%d", s.SegmentID, s.StartMs, s.EndMs)
		}
		if s.StartMs < prev {
			return fmt.Errorf("asr-raw: segment %s starts before previous segment ends (out of order)", s.SegmentID)
		}
		prev = s.EndMs
		for j, w := range s.Words {
			if w.Text == "" {
				return fmt.Errorf("asr-raw: segment %s word %d empty", s.SegmentID, j)
			}
			if w.StartMs < 0 || w.EndMs < w.StartMs {
				return fmt.Errorf("asr-raw: segment %s word %d has invalid range", s.SegmentID, j)
			}
		}
	}
	return nil
}

var sentEndRunes = map[rune]bool{'。': true, '！': true, '？': true,
	'!': true, '?': true, '；': true, ';': true, '…': true}

// PauseGapMs is the word-timestamp gap treated as a sentence break
// when punctuation is absent (ASR can drop 句末标点 entirely).
const PauseGapMs = 600

// SplitFusedSegments cuts segments that fused several sentences (a
// non-VAD backend can return one segment per whole chunk) so the block
// matcher — capped at a few lines per block — sees sentence-level
// input. Splits happen at terminal punctuation and, secondarily, at
// word-timestamp pauses ≥ PauseGapMs. Word-derived bounds are
// measured; every proportional text cut stays Coarse (a parent's real
// edges never legitimize an estimated interior cut). Segments that
// resist splitting pass through for the caller to degrade honestly.
func SplitFusedSegments(segs []ASRSegment) []ASRSegment {
	var out []ASRSegment
	for _, s := range segs {
		if len(s.Words) > 1 {
			var groups [][]ASRWord
			var cur []ASRWord
			prevEnd := -1
			for _, w := range s.Words {
				// A long inter-word silence is a sentence boundary even
				// without punctuation.
				if prevEnd >= 0 && len(cur) > 0 && w.StartMs-prevEnd >= PauseGapMs {
					groups = append(groups, cur)
					cur = nil
				}
				cur = append(cur, w)
				prevEnd = w.EndMs
				for _, r := range w.Text {
					if sentEndRunes[r] {
						groups = append(groups, cur)
						cur = nil
						break
					}
				}
			}
			if len(cur) > 0 {
				groups = append(groups, cur)
			}
			if len(groups) > 1 {
				for i, g := range groups {
					sub := s
					sub.SegmentID = fmt.Sprintf("%s.s%d", s.SegmentID, i+1)
					sub.Words = g
					var b strings.Builder
					for _, w := range g {
						b.WriteString(w.Text)
					}
					sub.Text = b.String()
					sub.StartMs = g[0].StartMs
					sub.EndMs = g[len(g)-1].EndMs
					if sub.EndMs <= sub.StartMs {
						sub.EndMs = sub.StartMs + 1
					}
					sub.Coarse = false // bounds are word-derived
					out = append(out, sub)
				}
				continue
			}
		}
		// Text-only fallback: split at punctuation, proportional bounds.
		// These interior cuts are estimates even when the parent's
		// outer bounds were measured — mark every piece coarse.
		rs := []rune(s.Text)
		var parts [][]rune
		var cur []rune
		for _, r := range rs {
			cur = append(cur, r)
			if sentEndRunes[r] {
				parts = append(parts, cur)
				cur = nil
			}
		}
		if len(cur) > 0 {
			parts = append(parts, cur)
		}
		if len(parts) <= 1 {
			out = append(out, s)
			continue
		}
		total := len(rs)
		pos := 0
		span := s.EndMs - s.StartMs
		for i, p := range parts {
			lo := s.StartMs + pos*span/total
			pos += len(p)
			hi := s.StartMs + pos*span/total
			if hi <= lo {
				hi = lo + 1
			}
			sub := s
			sub.SegmentID = fmt.Sprintf("%s.s%d", s.SegmentID, i+1)
			sub.Text = string(p)
			sub.Words = nil
			sub.StartMs, sub.EndMs = lo, hi
			sub.Coarse = true // proportional cut: always an estimate
			out = append(out, sub)
		}
	}
	return out
}
