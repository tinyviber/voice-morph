package dubbing

import (
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
