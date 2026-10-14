package dubbing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"voice-morph/engine"
)

type AnalyzeOptions struct {
	AudioPath  string
	ScriptPath string // optional; when set, reconcile runs and timeline.json is written
	OutDir     string
	ModelDir   string
	Language   string
	ChunkSec   int // Go-side fallback chunking window; 0 disables pre-chunking
	// BatchSizeS is FunASR's VAD merge/batch budget in seconds of audio
	// (CPU memory pressure scales with it). Default 120 per FunASR docs.
	BatchSizeS int
	NoVAD      bool
	ConfigPath string
	OnProgress func(ProgressEvent)
}

// Analyze runs ASR on the audio, writes asr-raw.json (raw evidence,
// never post-processed by script text), and — when a script is given —
// reconciles into timeline.json. Offline: nothing in this path ever
// triggers a model download.
func Analyze(opts AnalyzeOptions) (*ASRDoc, *Timeline, error) {
	if opts.Language == "" {
		opts.Language = "zh"
	}
	out, err := SessionDir(opts.OutDir)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(opts.AudioPath); err != nil {
		return nil, nil, fmt.Errorf("audio: %w", err)
	}
	sha, err := SHA256File(opts.AudioPath)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := LoadUserConfig(opts.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	ms, err := ResolveModel(cfg, opts.ModelDir)
	if err != nil {
		return nil, nil, err
	}
	if ms.State == "missing" {
		return nil, nil, fmt.Errorf("E_MODEL_NOT_FOUND: %s", ms.Detail)
	}
	if opts.NoVAD {
		ms.VADDir = ""
	}
	py, err := FindPython(cfg)
	if err != nil {
		return nil, nil, err
	}
	script, err := WorkerScriptPath()
	if err != nil {
		return nil, nil, err
	}
	w, err := SpawnWorker(SpawnOptions{Python: py, Script: script, Offline: true})
	if err != nil {
		return nil, nil, fmt.Errorf("E_WORKER_SPAWN: %w", err)
	}
	defer w.Close()
	if _, err := w.Hello(); err != nil {
		return nil, nil, fmt.Errorf("E_WORKER_FAIL: hello: %w", err)
	}
	lr, err := w.LoadModel(ms.ModelDir, ms.VADDir, "cpu", opts.Language, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("E_MODEL_INVALID: %w", err)
	}

	// Duration via decode (also validates the WAV early).
	src, err := LoadWAV48(opts.AudioPath)
	if err != nil {
		return nil, nil, fmt.Errorf("audio wav: %w", err)
	}
	durMs := int64(len(src)) * 1000 / engine.SampleRate

	// Fallback chunking only when the worker has no VAD and audio
	// exceeds the window; chunks land in out/chunks/ for auditability.
	var chunks []ASRChunk
	var results []*TranscribeResult
	needChunk := !lr.VAD && opts.ChunkSec > 0 && durMs > int64(opts.ChunkSec)*1000
	if !needChunk {
		r, err := w.Transcribe(opts.AudioPath, 0, opts.Language, lr.WordTimestamps,
			lr.VAD, 15, opts.batchSizeS(), true, durMs, opts.OnProgress)
		if err != nil {
			return nil, nil, fmt.Errorf("E_ASR: %w", err)
		}
		results = append(results, r)
	} else {
		if err := os.MkdirAll(filepath.Join(out, "chunks"), 0o755); err != nil {
			return nil, nil, err
		}
		env := energyRMS(src, engine.SampleRate*20/1000, engine.SampleRate*5/1000)
		hop := engine.SampleRate * 5 / 1000
		for off := 0; off*1 < len(src); {
			end := off + opts.ChunkSec*engine.SampleRate
			if end > len(src) {
				end = len(src)
			}
			// Cut at the lowest-energy point in the last 3s of window.
			if end < len(src) {
				end = refineBoundary(env, hop, end, 0, 3*engine.SampleRate, off+minCutSamp(engine.SampleRate), len(src))
			}
			wavPath := filepath.Join(out, "chunks", fmt.Sprintf("chunk-%03d.wav", len(chunks)))
			if err := SaveWAV(wavPath, slice(src, off, end)); err != nil {
				return nil, nil, err
			}
			offMs := int64(off) * 1000 / engine.SampleRate
			r, err := w.Transcribe(wavPath, offMs, opts.Language, lr.WordTimestamps,
				false, 15, opts.batchSizeS(), true,
				int64(end-off)*1000/engine.SampleRate, opts.OnProgress)
			if err != nil {
				return nil, nil, fmt.Errorf("E_ASR: chunk %d: %w", len(chunks), err)
			}
			results = append(results, r)
			chunks = append(chunks, ASRChunk{File: wavPath, OffsetMs: offMs, EndMs: int64(end) * 1000 / engine.SampleRate})
			off = end
		}
	}

	doc := &ASRDoc{
		Version: SchemaVersion, Audio: opts.AudioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: int(durMs),
		Backend: ASRBackend{Name: "funasr", ModelPath: ms.ModelDir, Language: opts.Language,
			WordTimestamps: lr.WordTimestamps, VAD: lr.VAD},
		Chunks: chunks,
	}
	for _, r := range results {
		doc.Segments = append(doc.Segments, r.Segments...)
		if len(r.Backend) > 0 {
			var extra map[string]any
			if json.Unmarshal(r.Backend, &extra) == nil {
				if v, ok := extra["funasr_version"].(string); ok {
					doc.Backend.Version = v
				}
			}
		}
	}
	sort.SliceStable(doc.Segments, func(i, j int) bool { return doc.Segments[i].StartMs < doc.Segments[j].StartMs })
	renumberSegments(doc.Segments)
	if err := doc.Validate(); err != nil {
		return nil, nil, fmt.Errorf("asr-raw invalid: %w", err)
	}
	asrPath := filepath.Join(out, "asr-raw.json")
	if err := SaveJSON(asrPath, doc); err != nil {
		return nil, nil, err
	}

	var tl *Timeline
	if opts.ScriptPath != "" {
		sc, err := LoadScript(opts.ScriptPath)
		if err != nil {
			return doc, nil, fmt.Errorf("E_SCRIPT_INVALID: %w", err)
		}
		tl, err = Reconcile(sc, doc, DefaultReconcileOptions())
		if err != nil {
			return doc, nil, err
		}
		tl.Audio = opts.AudioPath
		tl.AudioSHA256 = sha
		tl.SampleRate = engine.SampleRate
		tl.DurationMs = int(durMs)
		tl.Script = opts.ScriptPath
		tl.ASRFile = asrPath
		if err := SaveJSON(filepath.Join(out, "timeline.json"), tl); err != nil {
			return doc, tl, err
		}
	}
	return doc, tl, nil
}

func (o AnalyzeOptions) ChunkSecOrDefault() int {
	if o.ChunkSec > 0 {
		return o.ChunkSec
	}
	return 300
}

func (o AnalyzeOptions) batchSizeS() int {
	if o.BatchSizeS > 0 {
		return o.BatchSizeS
	}
	return 120
}

func minCutSamp(sr int) int { return sr / 2 }

func renumberSegments(segs []ASRSegment) {
	for i := range segs {
		segs[i].SegmentID = fmt.Sprintf("seg-%04d", i+1)
	}
}
