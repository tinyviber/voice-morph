# M0 evaluation speech samples

These two real-speech WAV files are checked in for reproducible offline
evaluation. They are not synthesized by this project and are not used to
claim any child-speech capability.

| File | Source | Speaker | Corpus sex label | Text |
|---|---|---:|---|---|
| `cmu-arctic-aew-a0004.wav` | `cmu_us_aew_arctic/wav/arctic_a0004.wav` in [cmu_us_aew_arctic.tar.bz2](http://festvox.org/cmu_arctic/packed/cmu_us_aew_arctic.tar.bz2) | `aew` | Male (CMU Arctic voice list / published speaker descriptions) | "Lord, but I'm glad to see you again, Phil." |
| `cmu-arctic-slt-a0003.wav` | `cmu_us_slt_arctic/wav/arctic_a0003.wav` in [cmu_us_slt_arctic.tar.bz2](http://festvox.org/cmu_arctic/packed/cmu_us_slt_arctic.tar.bz2) | `slt` | Female (CMU Arctic voice list / published speaker descriptions) | "For the twentieth time that evening the two men shook hands." |

Both files are original 16 kHz mono PCM WAV utterances, kept without
trimming or mastering. File sizes total about 191 KiB.

## License and attribution

`cmu_arctic_COPYING.txt` is the verbatim license fetched from
`http://festvox.org/cmu_arctic/cmu_arctic/cmu_us_slt_arctic/COPYING`
(the same CMU Arctic copyright/conditions apply to the sibling speaker
archives). It permits use, copying, modification and licensing for any
purpose, provided the copyright notice, conditions, disclaimer and
original author names are retained and modifications are marked.

- Dataset page/archives: <http://festvox.org/cmu_arctic/>
- Release note: <https://groups.google.com/g/comp.speech.research/c/JSwdwjrP38A>
- Paper: Kominek & Black, *The CMU ARCTIC speech databases*, SSW5 (2004),
  <https://www.cs.cmu.edu/~awb/papers/ssw5/arctic.pdf>
- `slt` female/US-English source metadata: MaryTTS `voice-cmu-slt`
  build metadata, <https://github.com/marytts/voice-cmu-slt/blob/master/build.gradle>
- `aew` male/CMU Arctic speaker list: <https://zenodo.org/records/3066489>
  and Kominek/Black speaker descriptions.

Attribution for these files: **Carnegie Mellon University, CMU ARCTIC
speech databases, speakers `aew` and `slt`**.

## Provenance notes

- Gender information is taken from corpus speaker metadata/descriptions,
  not inferred from names or voices.
- No child recording was included: the candidate open child-speech
  sources checked for M0 did not provide a clear, sufficiently compact,
  redistributable set of recordings with reliable consent/licensing
  metadata. The `natural-child` preset is therefore evaluated as an
  adult-source transformation target only.
- The local files are raw corpus utterances and have not been cleaned of
  natural pauses or mouth noises; report metrics should be interpreted
  accordingly.
