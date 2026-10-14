package evaluation

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"voice-morph/engine"
)

func TestSummarizeLevelsAndF0(t *testing.T) {
	x := make([]float32, 48000)
	for i := range x {
		x[i] = float32(0.25 * math.Sin(2*math.Pi*100*float64(i)/48000))
	}
	est := []PitchEstimate{{F0: 100, Voiced: true}, {F0: 110, Voiced: true}, {F0: 120, Voiced: true}}
	s := Summarize(x, 48000, est, 4)
	if math.Abs(s.RMS-0.17677) > 0.002 {
		t.Fatalf("RMS = %.6f", s.RMS)
	}
	if s.VoicedRatio != 0.75 || s.VoicedFrames != 3 || s.TotalFrames != 4 {
		t.Fatalf("voiced accounting = %#v", s)
	}
	if s.F0MedianHz != 110 || s.F0MinHz != 100 || s.F0MaxHz != 120 {
		t.Fatalf("f0 summary = %#v", s)
	}
	if math.Abs(s.SemitoneRange-(12*math.Log2(1.2))) > 1e-9 {
		t.Fatalf("range = %.8f", s.SemitoneRange)
	}
	if s.VoicedSeconds != 0.75 {
		t.Fatalf("VoicedSeconds = %v", s.VoicedSeconds)
	}
}

func TestProductionPitchAdapterFindsSine(t *testing.T) {
	const sr = 48000
	frame := make([]float64, 3072)
	for i := range frame {
		frame[i] = 0.2 * math.Sin(2*math.Pi*220*float64(i)/sr)
	}
	est := DefaultPitchTracker(sr).Analyze(frame)
	if !est.Voiced || est.Confidence <= 0 {
		t.Fatalf("sine not voiced: %#v", est)
	}
	if math.Abs(est.F0-220) > 3 {
		t.Fatalf("F0 = %.3f", est.F0)
	}
}

func TestProductionPitchAdapterRejectsSilence(t *testing.T) {
	est := DefaultPitchTracker(48000).Analyze(make([]float64, 3072))
	if est.Voiced || est.F0 != 0 {
		t.Fatalf("silence = %#v", est)
	}
}

func TestParseParamsValidation(t *testing.T) {
	if _, err := ParseParams([]byte(`{"pitch":0.5,"unknown":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := ParseParams([]byte(`{"pitch":0.5} {"pitch":0.6}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	p, err := ParseParams([]byte(`{"speech":true,"pitchRange":0.5,"targetF0":220,"formantShape":0.2,"pitch":0.4,"gain":1.2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Speech || p.PitchRange != 0.5 || p.TargetF0 != 220 || p.FormantShape != 0.2 || p.Gain != 1.2 {
		t.Fatalf("params lost: %#v", p)
	}
	if _, err := ParseParams([]byte(`{"pitch":2}`)); err == nil {
		t.Fatal("out-of-range pitch accepted")
	}
}

func TestRunWritesReportWAVAndBlindOutputs(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.wav")
	x := make([]float32, 48000/4)
	for i := range x {
		x[i] = float32(0.12 * math.Sin(2*math.Pi*180*float64(i)/48000))
	}
	if err := WriteWAVFile(inPath, x, 48000); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.json")
	blind := filepath.Join(dir, "blind.html")
	rep, err := Run(Options{InputPath: inPath, PresetID: "woman,natural-woman", OutDir: filepath.Join(dir, "wav"),
		ReportPath: report, BlindPath: blind, FrameSize: 3072, Hop: 512})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outputs) != 2 {
		t.Fatalf("outputs = %d", len(rep.Outputs))
	}
	for _, path := range []string{report, blind, rep.Outputs[0].WavPath, rep.Outputs[1].WavPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	// HTML blind listening copies audio under opaque names and never shows
	// preset names outside the hidden mapping details.
	entries, err := filepath.Glob(filepath.Join(dir, "sample-*.wav"))
	if err != nil || len(entries) != len(rep.Outputs)+1 { // outputs + original
		t.Fatalf("blind copies = %v, err %v", entries, err)
	}
	page, err := os.ReadFile(blind)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "查看盲听映射") {
		t.Fatal("blind page missing hidden mapping")
	}
	for _, o := range rep.Outputs {
		if strings.Contains(string(page), `src="`+filepath.Base(o.WavPath)) {
			t.Fatalf("blind page links %s by its condition name", o.WavPath)
		}
	}
	// The CSV manifest lists the original and every output with real paths.
	csvPath := filepath.Join(dir, "blind.csv")
	if err := WriteBlind(csvPath, rep); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != len(rep.Outputs)+2 || rows[0][0] != "blind_id" { // header + original + outputs
		t.Fatalf("blind CSV = %v, err %v", rows, err)
	}
	if rep.Input.DurationSec <= 0 || rep.Outputs[0].RTF <= 0 {
		t.Fatalf("bad timing report: %#v", rep)
	}
}

func TestUnknownPresetIsError(t *testing.T) {
	_, err := renderJobs(Options{PresetID: "does-not-exist"})
	if err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestParamsAndPresetConflict(t *testing.T) {
	p := DefaultEvalParams()
	if _, err := renderJobs(Options{Params: &p, PresetID: "woman"}); err == nil {
		t.Fatal("params+preset combination accepted")
	}
	if _, err := renderJobs(Options{ParamsPath: "/tmp/x.json", PresetID: "all"}); err == nil {
		t.Fatal("ParamsPath+preset all combination accepted")
	}
	if _, err := renderJobs(Options{Params: &p, ParamsPath: "/tmp/x.json"}); err == nil {
		t.Fatal("Params+ParamsPath combination accepted")
	}
}

func TestQualityNotesShortClip(t *testing.T) {
	s := SignalStats{DurationSec: 0.5, TotalFrames: 3, VoicedFrames: 2, VoicedSeconds: 0.1}
	if len(qualityNotes(s)) == 0 {
		t.Fatal("sparse clip produced no warnings")
	}
}

func TestDecodeInvalidWAVFails(t *testing.T) {
	if _, _, err := engine.DecodeWAV([]byte("not wave")); err == nil {
		t.Fatal("invalid WAV accepted")
	}
}

type countingTracker struct{ calls *int }

func (c countingTracker) Analyze([]float64) PitchEstimate {
	*c.calls++
	return PitchEstimate{}
}

// The voiced ratio is only meaningful if AnalyzePitch analyzes exactly the
// FrameCount frames it is divided by, including when the clip ends on a
// frame boundary.
func TestAnalyzePitchCoversFrameCount(t *testing.T) {
	const frame, hop = 3072, 512
	for _, n := range []int{1, frame - 1, frame, frame + 1, frame + 3*hop, frame + 3*hop + 7} {
		calls := 0
		AnalyzePitch(make([]float32, n), 48000, countingTracker{&calls}, frame, hop, 0)
		if want := FrameCount(n, frame, hop); calls != want {
			t.Errorf("%d samples: analyzed %d frames, FrameCount %d", n, calls, want)
		}
	}
}

// floatWAV encodes mono IEEE-float samples, which DecodeWAV accepts and
// which, unlike 16-bit PCM, can carry non-finite values.
func floatWAV(x []float32, sr int) []byte {
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + 4*len(x)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(3)) // IEEE float
	w(uint16(1)) // mono
	w(uint32(sr))
	w(uint32(4 * sr))
	w(uint16(4))
	w(uint16(32))
	b.WriteString("data")
	w(uint32(4 * len(x)))
	w(x)
	return b.Bytes()
}

func TestLoadWAVRejectsUnusableAudio(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"not-wave":   []byte("not wave"),
		"no-samples": engine.EncodeWAV(nil, 48000),
		"rate-100":   engine.EncodeWAV(make([]float32, 100), 100),
		"non-finite": floatWAV([]float32{0, float32(math.Inf(1))}, 48000),
	} {
		path := filepath.Join(dir, name+".wav")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadWAV(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := LoadWAV(filepath.Join(dir, "missing.wav")); err == nil {
		t.Error("missing file accepted")
	}
	if x, sr, err := LoadWAV(writeFloatWAV(t, dir, []float32{0.5, -0.25}, 44100)); err != nil || sr != 44100 || len(x) != 2 {
		t.Errorf("valid float WAV = %v, %d, %v", x, sr, err)
	}
}

func writeFloatWAV(t *testing.T, dir string, x []float32, sr int) string {
	t.Helper()
	path := filepath.Join(dir, "float.wav")
	if err := os.WriteFile(path, floatWAV(x, sr), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRejectsBadOptionsBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	if err := WriteWAVFile(in, make([]float32, 4800), 48000); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		o          Options
	}{
		{"negative frame", "must be positive", Options{FrameSize: -1}},
		{"hop over frame", "exceeds frame", Options{FrameSize: 1024, Hop: 2048}},
		{"huge frame", "exceeds one second", Options{FrameSize: 1 << 30}},
		{"confidence over 1", "min confidence", Options{MinConfidence: 1.5}},
		{"confidence NaN", "min confidence", Options{MinConfidence: math.NaN()}},
		{"blind extension", "blind output must be", Options{BlindPath: filepath.Join(dir, "out", "blind.txt")}},
		{"duplicate preset", "duplicate preset", Options{PresetID: "woman,woman"}},
		{"empty preset list", "no preset IDs", Options{PresetID: " , "}},
		{"missing params", "read params", Options{ParamsPath: filepath.Join(dir, "missing.json")}},
		{"missing reference", "read WAV", Options{ReferencePath: filepath.Join(dir, "missing.wav")}},
	} {
		tc.o.InputPath = in
		tc.o.ReportPath = filepath.Join(dir, "out", "report.json")
		if _, err := Run(tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("a rejected run wrote output: %v", err)
	}
}

func TestRunRefusesToOverwriteSource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "woman.wav")
	x := make([]float32, 4800)
	for i := range x {
		x[i] = float32(0.2 * math.Sin(2*math.Pi*200*float64(i)/48000))
	}
	if err := WriteWAVFile(in, x, 48000); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"render": {PresetID: "woman", OutDir: dir},
		"report": {PresetID: "normal", ReportPath: in},
	} {
		o.InputPath = in
		if _, err := Run(o); err == nil || !strings.Contains(err.Error(), "overwrite") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if after, err := os.ReadFile(in); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("input modified (err %v)", err)
	}
}

// Rendering and metrics are deterministic: two runs of the same real
// recording yield bit-identical WAVs and equal statistics; only timing
// fields (renderMs, totalMs, rtf, generatedAt) may differ.
func TestRunIsReproducibleOnRealSpeech(t *testing.T) {
	const in = "../testdata/cmu-arctic-slt-a0003.wav"
	run := func() InputReport {
		rep, err := Run(Options{InputPath: in, PresetID: "normal,woman,natural-woman", OutDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a.Input, b.Input) || !reflect.DeepEqual(a.Notes, b.Notes) {
		t.Fatalf("input metrics differ:\n%+v\n%+v", a.Input, b.Input)
	}
	if len(a.Outputs) != 3 || len(b.Outputs) != 3 {
		t.Fatalf("outputs = %d, %d", len(a.Outputs), len(b.Outputs))
	}
	for i, x := range a.Outputs {
		y := b.Outputs[i]
		if x.Name != y.Name || x.Params != y.Params || x.Stats != y.Stats {
			t.Fatalf("%s metrics differ:\n%+v\n%+v", x.Name, x.Stats, y.Stats)
		}
		wx, errX := os.ReadFile(x.WavPath)
		wy, errY := os.ReadFile(y.WavPath)
		if errX != nil || errY != nil || len(wx) == 0 || !bytes.Equal(wx, wy) {
			t.Fatalf("%s WAV not bit-identical (%v, %v)", x.Name, errX, errY)
		}
	}
	// The 16 kHz source is measured at the engine rate like the renders, so
	// its duration and pitch read true and the transparent preset matches
	// it; a rate mix-up would put a median about 3x off.
	x, sr, err := LoadWAV(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := float64(len(x)) / float64(sr); math.Abs(a.Input.DurationSec-want) > 0.01 {
		t.Fatalf("input duration %.3f s, source %.3f s", a.Input.DurationSec, want)
	}
	normal := a.Outputs[0]
	if f := a.Input.F0MedianHz; f < 150 || f > 250 || math.Abs(normal.Stats.F0MedianHz-f) > 1 {
		t.Fatalf("F0 median: input %.1f Hz, normal %.1f Hz", f, normal.Stats.F0MedianHz)
	}
}
