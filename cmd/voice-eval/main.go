// Command voice-eval renders one WAV through VoiceMorph presets and emits
// a reproducible JSON/audio/blind-listening bundle without Python or
// network dependencies.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"voice-morph/evaluation"
)

const usage = `voice-eval: offline VoiceMorph evaluation

Required:
  -in PATH              input WAV (mono/stereo PCM or float WAV, >= 8 kHz)

Rendering:
  -preset ID[,ID...]    preset(s), or "all" (default all)
  -params PATH          Params JSON rendered as "custom"; excludes -preset
  -out DIR              rendered WAV directory (default wav/ beside report)

Reports:
  -report PATH          JSON report path (default voice-eval-report.json)
  -blind PATH           .html local A/B page or .csv manifest
  -reference PATH       external reference/Praat WAV for comparison

Analysis (48 kHz samples; every clip is resampled to 48 kHz first):
  -frame N              F0 analysis frame (default 3072)
  -hop N                F0 hop, at most the frame (default 512)
  -min-confidence F     accepted voiced confidence 0..1 (default 0.35)

  -list-presets         print preset IDs and exit

Exit status: 0 success, 1 evaluation failed, 2 invalid command line.

Examples:
  voice-eval -in testdata/cmu-arctic-slt-a0003.wav -preset woman,natural-woman -report out/slt.json -blind out/slt.html
  voice-eval -in in.wav -params params.json -reference praat.wav -report out/run.json
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is the CLI body; it returns the process exit status.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("voice-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	var (
		in        = fs.String("in", "", "")
		preset    = fs.String("preset", "", "")
		params    = fs.String("params", "", "")
		out       = fs.String("out", "", "")
		report    = fs.String("report", "voice-eval-report.json", "")
		blind     = fs.String("blind", "", "")
		reference = fs.String("reference", "", "")
		frame     = fs.Int("frame", 3072, "")
		hop       = fs.Int("hop", 512, "")
		minConf   = fs.Float64("min-confidence", 0.35, "")
		list      = fs.Bool("list-presets", false, "")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the flag package already reported the error and usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "voice-eval: unexpected argument %q (pass the input as -in PATH, flags first)\n", fs.Arg(0))
		return 2
	}
	if *list {
		for _, p := range evaluation.Presets() {
			fmt.Fprintf(stdout, "%-14s %s / %s — %s\n", p.ID, p.Name, p.NameZh, p.Hint)
		}
		return 0
	}
	if strings.TrimSpace(*in) == "" {
		fmt.Fprintln(stderr, "voice-eval: -in is required")
		fs.Usage()
		return 2
	}
	rep, err := evaluation.Run(evaluation.Options{
		InputPath:     *in,
		PresetID:      *preset,
		ParamsPath:    *params,
		OutDir:        *out,
		ReportPath:    *report,
		BlindPath:     *blind,
		ReferencePath: *reference,
		FrameSize:     *frame,
		Hop:           *hop,
		MinConfidence: *minConf,
	})
	if err != nil {
		fmt.Fprintf(stderr, "voice-eval: %v\n", err)
		return 1
	}
	summary, err := json.MarshalIndent(struct {
		Input   evaluation.SignalStats `json:"input"`
		Outputs int                    `json:"outputs"`
		Report  string                 `json:"report"`
		Notes   []string               `json:"notes,omitempty"`
	}{rep.Input, len(rep.Outputs), *report, rep.Notes}, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "voice-eval: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(summary))
	return 0
}
