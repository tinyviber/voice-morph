package evaluation

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"voice-morph/engine"
)

// Options controls one offline evaluation run.
type Options struct {
	InputPath     string
	PresetID      string      // comma-separated preset IDs; "" or "all" renders every preset
	Params        *EvalParams // custom params instead of presets
	ParamsPath    string      // custom params JSON instead of presets
	OutDir        string      // rendered WAVs; "" means a wav/ dir beside the report (or blind file)
	ReportPath    string
	BlindPath     string // .html or .csv
	ReferencePath string // external Praat/reference WAV to compare
	Tracker       PitchTracker
	FrameSize     int              // F0 frame in 48 kHz samples; 0 selects 3072
	Hop           int              // F0 hop in 48 kHz samples; 0 selects 512
	MinConfidence float64          // 0..1
	Now           func() time.Time // test hook
}

// RenderedClip is one generated output and its measurements.
type RenderedClip struct {
	Name     string      `json:"name"`
	PresetID string      `json:"presetId,omitempty"`
	Params   EvalParams  `json:"params"`
	WavPath  string      `json:"wavPath"`
	Stats    SignalStats `json:"stats"`
	RenderMS float64     `json:"renderMs"`
	RTF      float64     `json:"rtf"`
	TotalMS  float64     `json:"totalMs"`
}

// InputReport aggregates everything a reproducible evaluation emits.
type InputReport struct {
	GeneratedAt string         `json:"generatedAt"`
	InputPath   string         `json:"inputPath"`
	Input       SignalStats    `json:"input"`
	Reference   *FileSummary   `json:"reference,omitempty"`
	Outputs     []RenderedClip `json:"outputs"`
	Notes       []string       `json:"notes,omitempty"`
}

// FileSummary is used for an external reference WAV (for example a Praat
// synthesis or manual target) without implying it was rendered here.
type FileSummary struct {
	Path  string      `json:"path"`
	Stats SignalStats `json:"stats"`
	Delta DeltaStats  `json:"delta"`
}

// DeltaStats is the compact comparison against the evaluated input.
type DeltaStats struct {
	F0MedianHz    float64 `json:"f0MedianHzDelta"`
	SemitoneRange float64 `json:"semitoneRangeDelta"`
	RMSDBFS       float64 `json:"rmsDbfsDelta"`
	PeakDBFS      float64 `json:"peakDbfsDelta"`
	VoicedRatio   float64 `json:"voicedRatioDelta"`
}

// Run evaluates one input WAV against one or more preset renders. It
// writes rendered WAVs, a JSON report and, optionally, a blind-listening
// artifact. Options, params, preset IDs and the reference are all checked
// before anything is rendered or written. No network or Python is used.
func Run(opts Options) (InputReport, error) {
	rep := InputReport{GeneratedAt: nowUTC(opts.Now), InputPath: opts.InputPath}
	if err := opts.validate(); err != nil {
		return rep, err
	}
	jobs, err := renderJobs(opts)
	if err != nil {
		return rep, err
	}
	if opts.OutDir == "" {
		// Rendered WAVs default to wav/ beside the report, or beside the
		// blind page, which links to them.
		if anchor := cmp.Or(opts.ReportPath, opts.BlindPath); anchor != "" {
			opts.OutDir = filepath.Join(filepath.Dir(anchor), "wav")
		}
	}
	if err := checkOutputs(opts, jobs); err != nil {
		return rep, err
	}
	// Input, reference and outputs are all measured at the engine rate so
	// one tracker configuration (lag bounds, frame length) applies to all.
	in, err := loadAtEngineRate(opts.InputPath)
	if err != nil {
		return rep, err
	}
	tracker := opts.Tracker
	if tracker == nil {
		tracker = DefaultPitchTracker(engine.SampleRate)
	}
	rep.Input = measure(in, tracker, opts.FrameSize, opts.Hop, opts.MinConfidence)
	inputNotes := qualityNotes(rep.Input)
	rep.Notes = append(rep.Notes, "F0 tracker: "+trackerName(tracker))
	rep.Notes = append(rep.Notes, inputNotes...)
	if opts.ReferencePath != "" {
		ref, err := CompareWAV(opts.ReferencePath, rep.Input, tracker, opts.FrameSize, opts.Hop, opts.MinConfidence)
		if err != nil {
			return rep, err
		}
		rep.Reference = &ref
		for _, note := range qualityNotes(ref.Stats) {
			rep.Notes = append(rep.Notes, "reference: "+note)
		}
	}

	for _, job := range jobs {
		clip, err := renderOne(in, job, opts, tracker)
		if err != nil {
			return rep, err
		}
		rep.Outputs = append(rep.Outputs, clip)
		// Every output whose measurement is weak is named; only notes the
		// input already carries (such as clip length) are not repeated.
		for _, note := range qualityNotes(clip.Stats) {
			if !slices.Contains(inputNotes, note) {
				rep.Notes = append(rep.Notes, "output "+clip.Name+": "+note)
			}
		}
	}
	if opts.ReportPath != "" {
		if err := WriteJSON(opts.ReportPath, rep); err != nil {
			return rep, err
		}
	}
	if opts.BlindPath != "" {
		if err := WriteBlind(opts.BlindPath, rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// validate rejects bad options before any audio is read or written, so a
// typo fails fast instead of after every preset has been rendered.
func (o Options) validate() error {
	if o.InputPath == "" {
		return errors.New("input WAV path is required")
	}
	if o.FrameSize < 0 || o.Hop < 0 {
		return errors.New("frame and hop must be positive (0 selects the default)")
	}
	frame, hop := framing(o.FrameSize, o.Hop)
	if frame > engine.SampleRate {
		return fmt.Errorf("frame %d exceeds one second (%d samples)", frame, engine.SampleRate)
	}
	if hop > frame {
		return fmt.Errorf("hop %d exceeds frame %d: analysis would skip samples", hop, frame)
	}
	if !(o.MinConfidence >= 0 && o.MinConfidence <= 1) {
		return fmt.Errorf("min confidence %g is outside [0,1]", o.MinConfidence)
	}
	if o.BlindPath != "" {
		if _, err := blindWriter(o.BlindPath); err != nil {
			return err
		}
	}
	return nil
}

type renderJob struct {
	name     string
	presetID string
	params   EvalParams
}

func renderJobs(opts Options) ([]renderJob, error) {
	if opts.Params != nil && opts.ParamsPath != "" {
		return nil, errors.New("use either Params or ParamsPath, not both")
	}
	if opts.Params != nil || opts.ParamsPath != "" {
		if opts.PresetID != "" {
			return nil, errors.New("-params cannot be combined with -preset")
		}
		p, err := customParams(opts)
		if err != nil {
			return nil, err
		}
		return []renderJob{{name: "custom", params: p}}, nil
	}
	if opts.PresetID == "" || opts.PresetID == "all" {
		presets := Presets()
		jobs := make([]renderJob, 0, len(presets))
		for _, p := range presets {
			jobs = append(jobs, renderJob{name: p.ID, presetID: p.ID, params: p.Params})
		}
		return jobs, nil
	}
	ids := splitList(opts.PresetID)
	if len(ids) == 0 {
		return nil, fmt.Errorf("no preset IDs in %q", opts.PresetID)
	}
	jobs := make([]renderJob, 0, len(ids))
	for i, id := range ids {
		// a repeated ID would overwrite its own WAV and blind sample
		if slices.Contains(ids[:i], id) {
			return nil, fmt.Errorf("duplicate preset %q", id)
		}
		p, ok := PresetByID(id)
		if !ok {
			return nil, fmt.Errorf("unknown preset %q (see -list-presets)", id)
		}
		jobs = append(jobs, renderJob{name: p.ID, presetID: p.ID, params: p.Params})
	}
	return jobs, nil
}

// customParams returns the validated Params or ParamsPath override.
func customParams(opts Options) (EvalParams, error) {
	if opts.Params != nil {
		return *opts.Params, ValidateParams(*opts.Params)
	}
	b, err := os.ReadFile(opts.ParamsPath)
	if err != nil {
		return EvalParams{}, fmt.Errorf("read params: %w", err)
	}
	return ParseParams(b)
}

func renderOne(in []float32, job renderJob, opts Options, tracker PitchTracker) (RenderedClip, error) {
	eng := engine.New()
	eng.SetParams(job.params)
	t0 := time.Now()
	out := eng.Render(in)
	renderMS := elapsedMS(t0)
	// totalMs adds the output analysis, so the report separates DSP cost
	// (renderMs, rtf) from evaluation cost.
	stats := measure(out, tracker, opts.FrameSize, opts.Hop, opts.MinConfidence)
	clip := RenderedClip{Name: job.name, PresetID: job.presetID, Params: job.params, Stats: stats,
		RenderMS: renderMS, TotalMS: elapsedMS(t0)}
	if stats.DurationSec > 0 {
		clip.RTF = (renderMS / 1000) / stats.DurationSec
	}
	if opts.OutDir != "" {
		clip.WavPath = wavPath(opts.OutDir, job.name)
		if err := WriteWAVFile(clip.WavPath, out, engine.SampleRate); err != nil {
			return clip, err
		}
	}
	return clip, nil
}

// wavPath is where a render job's WAV is written.
func wavPath(dir, name string) string { return filepath.Join(dir, safeName(name)+".wav") }

// checkOutputs refuses any destination that would overwrite the input or
// reference WAV, so a path mix-up cannot destroy the source recording.
func checkOutputs(opts Options, jobs []renderJob) error {
	dests := []string{opts.ReportPath, opts.BlindPath}
	if opts.OutDir != "" {
		for _, job := range jobs {
			dests = append(dests, wavPath(opts.OutDir, job.name))
		}
	}
	for _, src := range []string{opts.InputPath, opts.ReferencePath} {
		for _, dst := range dests {
			if samePath(src, dst) {
				return fmt.Errorf("output %s would overwrite source WAV %s", dst, src)
			}
		}
	}
	return nil
}

// samePath reports whether a and b are the same existing file, so links
// and case-insensitive names count. A destination that does not exist yet
// cannot overwrite anything.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// CompareWAV loads an external reference at the engine rate and reports
// the same metrics plus simple deltas against the evaluated input.
func CompareWAV(path string, input SignalStats, tracker PitchTracker, frame, hop int, minConf float64) (FileSummary, error) {
	x, err := loadAtEngineRate(path)
	if err != nil {
		return FileSummary{}, err
	}
	s := measure(x, tracker, frame, hop, minConf)
	return FileSummary{Path: path, Stats: s, Delta: DeltaStats{
		F0MedianHz:    s.F0MedianHz - input.F0MedianHz,
		SemitoneRange: s.SemitoneRange - input.SemitoneRange,
		RMSDBFS:       safeDeltaDB(s.RMSDBFS, input.RMSDBFS),
		PeakDBFS:      safeDeltaDB(s.PeakDBFS, input.PeakDBFS),
		VoicedRatio:   s.VoicedRatio - input.VoicedRatio,
	}}, nil
}

// measure computes the level and F0 statistics of a clip at the engine rate.
func measure(x []float32, tracker PitchTracker, frame, hop int, minConf float64) SignalStats {
	est := AnalyzePitch(x, engine.SampleRate, tracker, frame, hop, minConf)
	return Summarize(x, engine.SampleRate, est, FrameCount(len(x), frame, hop))
}

// LoadWAV reads and decodes a WAV file at its native sample rate. Audio
// that cannot yield meaningful statistics is rejected up front: empty
// clips, implausible rates (a corrupt header would otherwise drive an
// enormous resample) and NaN/Inf samples, which JSON cannot encode.
func LoadWAV(path string) ([]float32, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read WAV %s: %w", path, err)
	}
	x, sr, err := engine.DecodeWAV(b)
	if err != nil {
		return nil, 0, fmt.Errorf("decode WAV %s: %w", path, err)
	}
	if sr < 8000 || sr > 384000 {
		return nil, 0, fmt.Errorf("decode WAV %s: unsupported sample rate %d Hz", path, sr)
	}
	if len(x) == 0 {
		return nil, 0, fmt.Errorf("decode WAV %s: no audio samples", path)
	}
	for i, v := range x {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, 0, fmt.Errorf("decode WAV %s: sample %d is not finite", path, i)
		}
	}
	return x, sr, nil
}

// loadAtEngineRate loads a WAV and resamples it to engine.SampleRate.
func loadAtEngineRate(path string) ([]float32, error) {
	x, sr, err := LoadWAV(path)
	if err != nil || sr == engine.SampleRate {
		return x, err
	}
	return resampleToRate(x, sr, engine.SampleRate), nil
}

func resampleToRate(in []float32, from, to int) []float32 {
	r := engine.NewResampler(float64(to) / float64(from))
	x := make([]float64, len(in))
	for i, v := range in {
		x[i] = float64(v)
	}
	x = append(r.Process(x), r.Flush()...)
	out := make([]float32, len(x))
	for i, v := range x {
		out[i] = float32(v)
	}
	return out
}

func nowUTC(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return now().UTC().Format(time.RFC3339Nano)
}

func elapsedMS(t0 time.Time) float64 { return float64(time.Since(t0)) / 1e6 }

func trackerName(t PitchTracker) string {
	if _, ok := t.(pitchTrackerAdapter); ok {
		return "engine pitch tracker"
	}
	return fmt.Sprintf("%T", t)
}

func safeDeltaDB(a, b float64) float64 {
	if a <= -math.MaxFloat64/2 || b <= -math.MaxFloat64/2 {
		return 0
	}
	return a - b
}

// qualityNotes marks measurements that are too sparse to support a
// trustworthy F0/listening conclusion rather than letting a nearly empty
// percentile range look authoritative.
func qualityNotes(s SignalStats) []string {
	var notes []string
	if s.TotalFrames < 4 {
		notes = append(notes, "insufficient analysis frames for stable F0 statistics")
	}
	if s.VoicedFrames < 8 || s.VoicedSeconds < 0.25 {
		notes = append(notes, "too few voiced frames/seconds for reliable F0 range")
	}
	if s.DurationSec > 0 && s.DurationSec < 1 {
		notes = append(notes, "clip is shorter than 1 second; treat scores as diagnostic only")
	}
	return notes
}
