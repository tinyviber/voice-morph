package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"voice-morph/dubbing"
	"voice-morph/engine"
)

type dubberResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func parseDubberResponse(t *testing.T, payload string) dubberResponse {
	t.Helper()
	var r dubberResponse
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		t.Fatalf("response is not the standard JSON envelope: %v (%q)", err, payload)
	}
	return r
}

func TestDubberLoadTextFileDistinguishesMissingFromReadFailure(t *testing.T) {
	d := &Dubber{}
	dir := t.TempDir()
	path := filepath.Join(dir, "voices.json")
	content := `{"version":1}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var existing struct {
		Exists bool   `json:"exists"`
		Text   string `json:"text"`
	}
	r := parseDubberResponse(t, d.LoadTextFile(path))
	if !r.OK || json.Unmarshal(r.Data, &existing) != nil || !existing.Exists || existing.Text != content {
		t.Fatalf("existing file response = %+v, data=%s", r, r.Data)
	}

	var missing struct {
		Exists bool `json:"exists"`
	}
	r = parseDubberResponse(t, d.LoadTextFile(filepath.Join(dir, "missing.json")))
	if !r.OK || json.Unmarshal(r.Data, &missing) != nil || missing.Exists {
		t.Fatalf("missing file must be a successful, explicit absence: %+v, data=%s", r, r.Data)
	}

	r = parseDubberResponse(t, d.LoadTextFile(dir))
	if r.OK || r.Error == "" {
		t.Fatalf("read failure must remain an error, got %+v", r)
	}
}

func TestDubberCreateTextFileIfMissingDoesNotOverwrite(t *testing.T) {
	d := &Dubber{}
	path := filepath.Join(t.TempDir(), "voices.json")

	first := parseDubberResponse(t, d.CreateTextFileIfMissing(path, "first"))
	if !first.OK {
		t.Fatalf("create failed: %s", first.Error)
	}
	second := parseDubberResponse(t, d.CreateTextFileIfMissing(path, "replacement"))
	if second.OK {
		t.Fatal("second create unexpectedly replaced an existing file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("existing content overwritten: %q", got)
	}
}

func TestDubberRenderReportsManifestWriteFailure(t *testing.T) {
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "take.wav")
	samples := make([]float32, engine.SampleRate)
	for i := range samples {
		samples[i] = float32(0.25 * math.Sin(2*math.Pi*180*float64(i)/engine.SampleRate))
	}
	if err := os.WriteFile(audioPath, engine.EncodeWAV(samples, engine.SampleRate), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := dubbing.SHA256File(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	tl := &dubbing.Timeline{
		Version: dubbing.SchemaVersion, Audio: audioPath, AudioSHA256: sha,
		SampleRate: engine.SampleRate, DurationMs: 1000,
		Segments: []dubbing.TimelineSegment{{
			SegmentID: "seg-0001", LineID: "l01", RoleID: "narrator",
			ASRRaw: "你好", SpokenText: "你好", StartMs: 100, EndMs: 900,
			MatchStatus: dubbing.MatchExact, Selected: true,
		}},
	}
	tlPath := filepath.Join(dir, "timeline.json")
	if err := dubbing.SaveJSON(tlPath, tl); err != nil {
		t.Fatal(err)
	}
	voicesPath := filepath.Join(dir, "voices.json")
	voices := `{"version":1,"voices":{"narrator":{"backend":"dsp","preset":"normal"}}}`
	if err := os.WriteFile(voicesPath, []byte(voices), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	manifestPath := filepath.Join(outDir, "manifest.json")
	if err := os.MkdirAll(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}

	response := parseDubberResponse(t, (&Dubber{}).Render(tlPath, voicesPath, outDir))
	if response.OK || response.Error == "" {
		t.Fatalf("manifest write failure must reach the GUI, got %+v", response)
	}
	if _, err := os.Stat(filepath.Join(outDir, "mix.wav")); err != nil {
		t.Fatalf("render should have completed before the manifest write failed: %v", err)
	}
}
