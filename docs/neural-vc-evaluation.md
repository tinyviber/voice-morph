# Offline neural voice-conversion evaluation (M3 exploration, 2026-10-14)

This note records an offline comparison of VoiceMorph's DSP path against
[Seed-VC v1](https://github.com/Plachtaa/seed-vc) (PyPI `seed-vc` 0.4.3,
GPL-3.0) on real Chinese speech. Its purpose was to decide whether a
neural path is worth pursuing — not to pick final parameters, and not to
ship any converted audio.

**Nothing in this note is versioned in the repository.** The runs live in
a throwaway directory outside the repo (`/tmp/vm-nn/`). No podcast audio,
no converted output, no model weights and no Seed-VC code are committed
here — the source material is copyrighted and the speakers' voices are
protected, so this note only describes the setup and the verdicts. All
model weights came from ModelScope mirrors and every Seed-VC run executed
inside a network-denied sandbox with the HuggingFace hub offline, so no
audio or credentials left the machine.

## Setup

- **Sources**: two male excerpts and one female excerpt (11–17 s each)
  from the podcasts 故事FM E911/E896, plus a 7.75 s short phrase and a
  21.97 s alternate reference cut from the same speaker. Sources were
  attenuated −6 dB before DSP rendering so the engine's soft-clip knee
  (0.9) never enters the comparison.
- **Models** (CPU float32, seed 0):
  - `DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned` — 22.05 kHz
    voice-conversion model, no F0 conditioning.
  - `DiT_seed_v2_uvit_whisper_base_f0_44k_bigvgan_pruned_ft_ema_v2` —
    44.1 kHz model **built for singing voice conversion** (RMVPE F0
    conditioning). Defaults: 30 diffusion steps, CFG 0.7.
- **Pitch controls**: `auto-f0` aligns the source median log-F0 to the
  reference median, then a semitone shift is added on top. A
  `--target-f0` mode pins the conditioned median explicitly.
- **Pages**: locally generated blind pages (opaque `S<hash>` IDs, labels
  behind a fold); 48 kHz playback, active-speech RMS aligned at
  −23 dBFS, peaks capped at −1 dB. This is level matching, not loudness
  matching, and a 22 kHz output keeps its bandwidth difference.

## Round 1 — screen all paths (single-blind, 23 clips)

| Verdict | Conditions |
|---|---|
| Good | 22 kHz VC; 44.1k auto-F0 **+4 st**; originals |
| Weird articulation / "AI" | 44.1k auto-F0 **+8 st** (both sources); the female +8 converted *to a same-speaker reference* sounded processed |
| Audible electronics | legacy DSP `woman` (mild on male speech, 轻微电音), legacy `child` |
| Obvious artifacts — dropped | DSP `natural-woman`, `natural-child`; `child` on the female source (pitch "unlike a human") |

Read of round 1, after correcting earlier over-claims (see caveats
below): the artifacts users notice in the DSP presets are not a pitch
problem — they are spectral-envelope and PSOLA-artifact problems.
>+8 semitones *on top of auto-F0 alignment* on the male source is roughly
a 12-semitone net shift and produced the clearest articulation issues,
but it is a dose, not a hard boundary.

## Round 2 — one-factor controls (two pages, 22 clips)

Design in `/tmp/vm-nn/docs/round2-experiment.md`; every run was a single
context chunk, verified conditioned medians tracked the requested
targets (±0.02 Hz), and no frame exceeded the 1103 Hz F0-quantization
quirk in `seed_vc/length_regulator.py`.

| Group | Axes | Conditions |
|---|---|---|
| Pitch, 44.1k auto-F0 | semitones | male +4 / +6 / +8; female +2 / +4 / +8 |
| Voice conversion | model | female 22 kHz VC alongside 44.1k |
| Reference length | seconds of same speaker | 10.25 s vs 21.97 s, target F0 fixed at 211.4 Hz |
| Classifier-free guidance | CFG | 0 / 0.3 / 0.7 |
| Diffusion steps | steps | 30 vs 50 |

**User verdict: every variant sounded acceptable.** With a single
listener, that means the discriminative axis in this experiment is
roughly the audibility floor reached around 30 steps/CFG 0.7/short
reference — none of the lowered settings (CFG 0, CFG 0.3, 30 steps,
10.25 s reference) produced an obvious failure either. Round-2 clips
are short and single-condition; they cannot tell +6 from +8 as
consistently as the longer round-1 sentences did.

## What this means for VoiceMorph

1. **The DSP path's remaining complaints are timbre artifacts, not
   reachable by more pitch-law tuning.** The neural condition that keeps
   naturalness across +4…+8 shifts is exactly what a spectral-envelope
   warp cannot deliver; this supports keeping M3 as the direction.
2. **Offline first is safe.** Conversion ran at ~5.8–11.3× the clip's
   duration on this machine (Intel i7-14700KF, CPU float32, 30 steps;
   e.g. ~86 s for the 7.7 s clip, ~195 s for the 17.3 s one, ~91 s for
   the 15.7 s 22 kHz model), and ~25× with a 22 s reference — so neural
   VC is an offline render or a longer-pipeline feature, not a drop-in
   for the ~107 ms monitor path.
3. **Cheapest operating point found** so far: 44.1k F0 model,
   auto-F0 +4 st on male sources, CFG 0.7, 30 steps, ~10 s reference.
   That is a starting point for a quality floor test, not a conclusion —
   the knobs were only sampled once each and only short clips were
   heard.

## Caveats and open issues

- **Not a strong blind test**: one listener, small n, sources are
  copyrighted podcasts rather than target users' voices, and there is
  still **no child reference** — adult→child quality is unvalidated.
- **Model provenance**: Seed-VC is GPL-3.0 research code; the 44.1k
  checkpoint is tuned for singing. Either fact alone could bar a
  productized integration; this experiment does not settle licensing or
  latency, only that offline quality clears the DSP path's issues.
- **Confirmed-only framing**: the earlier write-up
  (`/tmp/vm-nn/docs/parameter-diagnosis-2026-10-14.md`) overclaimed some
  boundaries (e.g. "±6 st breaks", "10 s reference too short",
  "reference median = pitch ceiling"). Round 2 deliberately re-tested
  those and none held up as universal limits; what remains true is that
  larger net shifts degrade articulation.
- The 22 kHz output retains a bandwidth limitation you can hear on
  headphones; level-matching does not hide it.

## Next candidates if M3 proceeds

- Repeat the floor test with **sentence-length sources** (the current
  clips end mid-paragraph in one case) and a **second listener**, then
  try `steps=20`/`cfg=0.3` to find where quality actually collapses.
- A child speaker target needs licensed child speech before any
  child-voice claim can be evaluated.
- Any product decision also needs a license answer for GPL-3.0 Seed-VC
  (or an alternative checkpoint) before code enters this repository.
