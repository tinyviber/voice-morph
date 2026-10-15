// Package dubbing implements the offline multi-character dubbing pipeline
// for VoiceMorph:
//
//	recording WAV ──ASR──> asr-raw.json ──reconcile──> timeline.json
//	                                                        │ review/edit
//	                        voices.json ──render──> dubbed WAV + tracks
//	                                               + subtitles + manifest
//
// Audio is the source of truth: the script only guides reconciliation, it
// never overwrites what the ASR actually heard. Everything on disk is a
// versioned JSON document so each stage can be re-run, cached and
// inspected independently, and so external agents can drive the whole
// pipeline through cmd/voice-dub without the desktop UI.
package dubbing

// SchemaVersion is the current on-disk schema version for every dubbing
// document. Loaders reject anything newer.
const SchemaVersion = 1
