package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"voice-morph/dubbing"
	"voice-morph/engine"

	"github.com/egoist/mygo"
)

// Dubber is the bound service behind the 离线配音 workspace. Methods
// take and return plain JSON strings so the generated mygo bridge
// stays simple; the Go side always validates before touching disk.
type Dubber struct{}

func okJSON(v any) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "data": v})
	return string(b)
}

func errJSON(err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return string(b)
}

// PickFile opens a native open dialog; exts is a comma list like
// "wav,json" (empty = any). Returns "" when cancelled.
func (d *Dubber) PickFile(title, exts string) string {
	opts := mygo.OpenDialogOptions{Title: title}
	if exts != "" {
		opts.Filters = []mygo.FileFilter{{Name: exts, Extensions: strings.Split(exts, ",")}}
	}
	files, err := mygo.Dialog.Open(opts)
	if err != nil || len(files) == 0 {
		return ""
	}
	return files[0]
}

func (d *Dubber) PickDirectory(title string) string {
	files, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: title, Directory: true, CreateDirectories: true})
	if err != nil || len(files) == 0 {
		return ""
	}
	return files[0]
}

// ModelStatus reports the resolved model (or "missing").
func (d *Dubber) ModelStatus() string {
	cfg, err := dubbing.LoadUserConfig("")
	if err != nil {
		return errJSON(err)
	}
	ms, err := dubbing.ResolveModel(cfg, "")
	if err != nil {
		return errJSON(err)
	}
	return okJSON(ms)
}

// ScanModels lists every candidate dir with its evidence.
func (d *Dubber) ScanModels() string {
	cfg, err := dubbing.LoadUserConfig("")
	if err != nil {
		return errJSON(err)
	}
	return okJSON(dubbing.ScanModels(cfg))
}

// SetModel validates + persists a model dir picked in the UI.
func (d *Dubber) SetModel(dir, vadDir string) string {
	cfg, err := dubbing.LoadUserConfig("")
	if err != nil {
		return errJSON(err)
	}
	nd, err := dubbing.NormalizePath(dir)
	if err != nil {
		return errJSON(err)
	}
	c := dubbing.ValidateModelDir(nd, "sensevoice")
	if !c.Valid {
		return errJSON(fmt.Errorf("%s", c.Detail))
	}
	cfg.ModelDir = nd
	if vadDir != "" {
		if nv, err := dubbing.NormalizePath(vadDir); err == nil {
			cfg.VADDir = nv
		}
	}
	if err := cfg.Save(""); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]string{"model_dir": nd, "vad_dir": cfg.VADDir})
}

// Analyze runs ASR (+reconcile when script is non-empty). It blocks
// for the duration — the UI awaits it and shows progress text.
func (d *Dubber) Analyze(audio, script, outDir, modelDir string) string {
	doc, tl, err := dubbing.Analyze(dubbing.AnalyzeOptions{
		AudioPath: audio, ScriptPath: script, OutDir: outDir,
		ModelDir: modelDir, Language: "zh", OnProgress: nil})
	if err != nil {
		return errJSON(err)
	}
	r := map[string]any{"asr_raw": filepath.Join(outDir, "asr-raw.json"), "segments": len(doc.Segments)}
	if tl != nil {
		r["timeline"] = filepath.Join(outDir, "timeline.json")
		r["review_pending"] = tl.ReviewCount
		r["timeline_doc"] = tl
	}
	return okJSON(r)
}

// LoadTimelineFile returns a validated timeline for the review UI.
func (d *Dubber) LoadTimelineFile(path string) string {
	tl, err := dubbing.LoadTimeline(path)
	if err != nil {
		return errJSON(err)
	}
	return okJSON(tl)
}

// SaveTimelineFile persists the user's reviewed timeline. The payload
// must be a complete, valid timeline JSON document.
func (d *Dubber) SaveTimelineFile(path, payload string) string {
	tl, err := dubbing.ParseTimeline([]byte(payload))
	if err != nil {
		return errJSON(err)
	}
	tl.RefreshReviewCount()
	if err := dubbing.SaveJSON(path, tl); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]string{"saved": path})
}

// Render runs the batch render and returns the manifest.
func (d *Dubber) Render(timelinePath, voicesPath, outDir string) string {
	m, err := dubbing.Render(dubbing.RenderOptions{
		TimelinePath: timelinePath, VoicesPath: voicesPath, OutDir: outDir,
		WriteSrc: true})
	if err != nil && m == nil {
		return errJSON(err)
	}
	return okJSON(m)
}

// ReadFileB64 lets the webview play per-segment audio from disk.
func (d *Dubber) ReadFileB64(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// LoadTextFile reads a UTF-8 text file (voices.json, script.json).
func (d *Dubber) LoadTextFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// SaveTextFile writes a UTF-8 text file atomically.
func (d *Dubber) SaveTextFile(path, payload string) string {
	if err := dubbing.WriteFileAtomic(path, []byte(payload), 0o644); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]string{"saved": path})
}

// DefaultVoices returns a starter voices.json for the session.
func (d *Dubber) DefaultVoices() string {
	return `{
  "version": 1,
  "voices": {
    "narrator": {"backend": "dsp", "preset": "normal"},
    "character-a": {"backend": "dsp", "preset": "low"},
    "character-b": {"backend": "dsp", "preset": "natural-woman"}
  }
}
`
}

// PresetList exposes engine presets so the UI can label them honestly
// (experimental ones stay marked 实验).
func (d *Dubber) PresetList() string {
	return okJSON(engine.Presets)
}

// OpenPath reveals a file/dir in the OS file manager.
func (d *Dubber) OpenPath(path string) string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return errJSON(err)
	}
	return okJSON(true)
}

// Message shows a native message box (e.g. model missing guidance).
func (d *Dubber) Message(title, content string) {
	mygo.Dialog.Message(mygo.MessageOptions{
		Type: mygo.MessageInfo, Title: title, Message: content,
	})
}
