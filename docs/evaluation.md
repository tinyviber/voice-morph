# Offline evaluation (`voice-eval`)

`cmd/voice-eval` is a pure-Go, offline evaluation runner for VoiceMorph.
It decodes WAV through `engine.DecodeWAV`, resamples with
`engine.NewResampler`, renders through production `engine.Presets` or an
explicit production `engine.Params` JSON object, and emits:

- rendered WAV files,
- one JSON report containing input/output signal and timing metrics,
- an optional local blind A/B HTML page or CSV manifest,
- optional comparison against an external reference WAV (for example a
  Praat-generated target).

No Python, neural model, network service or external audio tool is used.
The F0 measurements use `engine.NewPitchTracker` so report metrics and
speech-path rendering share the same estimator.

## Quick start

```sh
go build ./cmd/voice-eval

# Compare the shipped real-speech samples over every production preset.
./voice-eval -in testdata/cmu-arctic-slt-a0003.wav -preset all \
  -report /tmp/voice-eval/slt/report.json \
  -blind /tmp/voice-eval/slt/index.html

# A/B legacy "woman" against the new natural speech preset.
./voice-eval -in testdata/cmu-arctic-aew-a0004.wav \
  -preset woman,natural-woman \
  -report /tmp/voice-eval/aew-woman/report.json \
  -blind /tmp/voice-eval/aew-woman/index.html

# Render an explicit Params JSON and compare with a Praat/reference WAV.
./voice-eval -in input.wav -params params.json -reference praat-target.wav \
  -report /tmp/voice-eval/custom/report.json \
  -blind /tmp/voice-eval/custom/blind.csv
```

`./voice-eval -list-presets` prints the preset IDs. Rendered WAVs go to
`-out`, by default a `wav/` directory beside the report.

`-params` accepts the same fields as `engine.Params`: `pitch`, `timbre`,
`strength`, `gain`, `bypass`, `eqPre`, `eqPost`, `speech`, `pitchRange`,
`targetF0` and `formantShape`. The JSON root must be an object; unknown
fields, malformed values and non-finite/out-of-range values are errors.
Omitted fields use `engine.DefaultParams` (`strength=1`, `gain=1`,
`pitchRange=1`, `targetF0=0` automatic). It renders one condition named
`custom` and cannot be combined with `-preset`.

## Errors and exit status

The exit status is 0 on success, 1 when the evaluation fails, and 2 for
an invalid command line (unknown flag, malformed value, missing `-in`,
stray argument). Errors and `-h` usage go to stderr; stdout carries only
the JSON summary, or the preset list for `-list-presets`. Options,
presets and params are checked before any audio is rendered or written.
Rejected inputs:

- unreadable or undecodable WAV, a WAV without samples, a sample rate
  outside 8–384 kHz, or non-finite samples (input and `-reference`);
- unknown, duplicate or empty `-preset` IDs; `-params` together with
  `-preset`;
- `-hop` larger than `-frame`, negative values, a frame over one second,
  `-min-confidence` outside 0..1, a `-blind` path that is not `.html`,
  `.htm` or `.csv`;
- a report, blind page or rendered WAV path that is the input or
  reference WAV itself.

## Reproducibility

Rendering and analysis are deterministic: repeating a run produces
bit-identical WAVs and identical `SignalStats`. Only `generatedAt`,
`renderMs`, `totalMs` and `rtf` vary between runs. The tests in
`evaluation/` and `cmd/voice-eval/` check this on the bundled recording.

Every clip (input, renders, reference) is resampled to the engine rate
(48 kHz) before analysis, so one tracker configuration measures all of
them; `-frame`/`-hop` are in 48 kHz samples whatever the input rate.

## Report schema

Top level:

- `generatedAt` — UTC RFC3339 timestamp.
- `inputPath` / `input` — source path and `SignalStats`.
- `reference` — optional path, `SignalStats`, and deltas versus input.
- `outputs[]` — one rendered condition per preset/custom params.
- `notes[]` — tracker identity and sparse-data warnings.

`SignalStats` reports:

- `durationSec`, `sampleRate`, `samples`;
- `rms`, `rmsDbfs`, `peak`, `peakDbfs`;
- `voicedRatio`, `voicedFrames`, `totalFrames`, `voicedSeconds`;
- `f0MedianHz`, `f0P10Hz`, `f0P90Hz`, `f0MinHz`, `f0MaxHz`;
- `semitoneRange`, `medianMidi`, `p10Midi`, `p90Midi`.

Each `outputs[]` additionally has `name`, `presetId`, exact `params`,
`wavPath`, `renderMs`, `totalMs`, and `rtf = renderSeconds /
durationSec`. `totalMs` adds the output's pitch analysis to the render;
`rtf` isolates the DSP render.

The evaluator explicitly marks weak measurements in `notes[]` when the
clip has too few analysis frames, too little voiced time, or is under
one second, naming each affected output (a note the input already
carries is not repeated per output). Those outputs are useful for smoke
tests but should not be read as stable voice-quality scores.

## Blind listening

`-blind PATH.html` writes a self-contained local page. Every clip is
copied beside it under an opaque `sample-<id>.wav` name, so neither the
labels nor the audio file names reveal the condition; the mapping is
hidden behind a `<details>` section so listeners can rate A/B before
revealing labels. `-blind PATH.csv` writes the same manifest (with the
real WAV paths) for spreadsheet tracking.
Use `-preset woman,natural-woman` (or comma-separated equivalents) to
compare the legacy alias against the speech-aware natural target.

## Bundled real-speech data

`testdata/` currently contains two CMU Arctic utterances:

- `cmu-arctic-aew-a0004.wav` — speaker `aew`, corpus-labelled male,
  16 kHz mono, ~2.9 s.
- `cmu-arctic-slt-a0003.wav` — speaker `slt`, corpus-labelled female,
  16 kHz mono, ~3.2 s.

See `testdata/README.md` for source URLs, attribution, license notes and
the reason no child recording is bundled.

## Known limitations

- The F0 tracker searches 60–600 Hz. Low male voices can produce frames
  pinned at the 60 Hz floor (creak or sub-harmonic picks); on
  `cmu-arctic-aew-a0004.wav` the input P10 and minimum read 60.0 Hz,
  which widens the reported semitone range and can lower the input
  median. Read `f0MinHz`/`f0P10Hz` at 60 Hz as a tracker bound, not a
  measured pitch.
- The report holds clip-level statistics only, with no per-frame F0
  track. Within-utterance effects such as the speech path's causal F0
  center (a running median) lagging a declining contour (natural presets
  landing below `targetF0`) show up only as summary offsets.
- `semitoneRange` spans `f0MinHz`–`f0MaxHz`, so one transition frame can
  dominate it: on `slt` the `natural-child` render reads ~22 st only
  because a single voiced-onset frame (dry 200 Hz fading into wet
  278 Hz) tracks at 93 Hz. Compare `p10Midi`/`p90Midi` for intonation
  spread.
- `voicedRatio` on rendered speech counts frames the same tracker still
  finds periodic. A drop against the input is a useful roughness signal,
  but it is not a perceptual score; confirm with the blind page.
- Two short adult utterances are bundled; there is no child recording,
  so `natural-child` is evaluated only as a transformation of adult
  speech.
