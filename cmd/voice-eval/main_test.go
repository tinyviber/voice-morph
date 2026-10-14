package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"voice-morph/evaluation"
)

const sample = "../../testdata/cmu-arctic-slt-a0003.wav"

func TestUsageDocumentsRequiredFlags(t *testing.T) {
	for _, want := range []string{"-in PATH", "-preset ID[,ID...]", "-params PATH", "-reference PATH", ".html local A/B page or .csv", "-list-presets", "Exit status"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("usage missing %q", want)
		}
	}
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

// Command-line mistakes exit 2, evaluation failures exit 1; both report on
// stderr only and never write a report.
func TestCLIErrors(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "out", "report.json")
	bad := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(bad, []byte("not wave"), 0o644); err != nil {
		t.Fatal(err)
	}
	params := filepath.Join(dir, "params.json")
	if err := os.WriteFile(params, []byte(`{"pitch":0.2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		code int
		want string
		args []string
	}{
		{"missing -in", 2, "-in is required", nil},
		{"unknown flag", 2, "flag provided but not defined", []string{"-bogus"}},
		{"malformed number", 2, "invalid value", []string{"-in", sample, "-hop", "x"}},
		{"stray argument", 2, "unexpected argument", []string{"-in", sample, "extra"}},
		{"missing input", 1, "read WAV", []string{"-in", filepath.Join(dir, "missing.wav")}},
		{"undecodable input", 1, "decode WAV", []string{"-in", bad}},
		{"unknown preset", 1, `unknown preset "nope"`, []string{"-in", sample, "-preset", "nope"}},
		{"params with preset", 1, "cannot be combined", []string{"-in", sample, "-params", params, "-preset", "woman"}},
		{"bad blind extension", 1, "blind output must be", []string{"-in", sample, "-blind", filepath.Join(dir, "b.txt")}},
		{"bad confidence", 1, "min confidence", []string{"-in", sample, "-min-confidence", "2"}},
	} {
		code, stdout, stderr := runCLI(append(tc.args, "-report", report)...)
		if code != tc.code || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit %d with %q", tc.name, code, stdout, stderr, tc.code, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("a failed run wrote output: %v", err)
	}
}

func TestCLIHelpAndListPresets(t *testing.T) {
	if code, stdout, stderr := runCLI("-h"); code != 0 || stdout != "" || !strings.Contains(stderr, "voice-eval: offline") {
		t.Fatalf("-h: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, _ := runCLI("-list-presets")
	if code != 0 {
		t.Fatalf("-list-presets exit %d", code)
	}
	for _, p := range evaluation.Presets() {
		if !strings.Contains(stdout, p.ID) {
			t.Fatalf("-list-presets missing %s", p.ID)
		}
	}
}

// A real-speech run writes the report, the renders beside it and a blind
// page, and prints a summary that matches the report. Repeating the run
// reproduces every rendered WAV byte for byte.
func TestCLIEndToEndReproducible(t *testing.T) {
	type summary struct {
		Input   evaluation.SignalStats `json:"input"`
		Outputs int                    `json:"outputs"`
		Report  string                 `json:"report"`
	}
	runOnce := func() (summary, evaluation.InputReport) {
		dir := t.TempDir()
		report := filepath.Join(dir, "report.json")
		code, stdout, stderr := runCLI("-in", sample, "-preset", "woman,natural-woman",
			"-report", report, "-blind", filepath.Join(dir, "index.html"))
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		var s summary
		if err := json.Unmarshal([]byte(stdout), &s); err != nil {
			t.Fatalf("stdout is not the JSON summary: %v\n%s", err, stdout)
		}
		b, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var rep evaluation.InputReport
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatal(err)
		}
		if s.Outputs != 2 || len(rep.Outputs) != 2 || s.Report != report || !reflect.DeepEqual(s.Input, rep.Input) {
			t.Fatalf("summary %+v does not match report %+v", s, rep.Input)
		}
		for _, o := range rep.Outputs {
			if filepath.Dir(o.WavPath) != filepath.Join(dir, "wav") {
				t.Fatalf("render %s not in wav/ beside the report", o.WavPath)
			}
		}
		return s, rep
	}
	s1, r1 := runOnce()
	s2, r2 := runOnce()
	if !reflect.DeepEqual(s1.Input, s2.Input) {
		t.Fatal("input metrics differ between runs")
	}
	for i := range r1.Outputs {
		a, errA := os.ReadFile(r1.Outputs[i].WavPath)
		b, errB := os.ReadFile(r2.Outputs[i].WavPath)
		if errA != nil || errB != nil || !bytes.Equal(a, b) || r1.Outputs[i].Stats != r2.Outputs[i].Stats {
			t.Fatalf("%s not reproducible (%v, %v)", r1.Outputs[i].Name, errA, errB)
		}
	}
}
