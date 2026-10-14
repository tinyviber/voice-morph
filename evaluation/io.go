package evaluation

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"voice-morph/engine"
)

// WriteJSON writes the report with stable indentation and permissions.
func WriteJSON(path string, rep InputReport) error {
	if path == "" {
		return errors.New("report path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// WriteWAVFile writes rendered mono samples through engine.EncodeWAV.
func WriteWAVFile(path string, samples []float32, sr int) error {
	if path == "" {
		return errors.New("output WAV path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	return os.WriteFile(path, engine.EncodeWAV(samples, sr), 0o644)
}

// WriteBlind emits either a standalone local HTML A/B page or a CSV
// manifest, selected by the destination extension. The page labels audio
// only by opaque blind IDs; the condition mapping is in a collapsed
// section the listener reveals manually.
func WriteBlind(path string, rep InputReport) error {
	write, err := blindWriter(path)
	if err != nil {
		return err
	}
	return write(path, rep)
}

// blindWriter selects the writer for a blind destination by extension.
func blindWriter(path string) (func(string, InputReport) error, error) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".csv":
		return writeBlindCSV, nil
	case ".html", ".htm":
		return writeBlindHTML, nil
	default:
		return nil, fmt.Errorf("blind output must be .html or .csv, got %q", ext)
	}
}

type blindRow struct {
	ID        string
	Condition string
	File      string
	F0Median  float64
	RMSDB     float64
	PeakDB    float64
	RTF       float64
}

func blindRows(rep InputReport) []blindRow {
	rows := make([]blindRow, 0, len(rep.Outputs)+2)
	if rep.InputPath != "" {
		rows = append(rows, blindRow{ID: blindID("in", rep.InputPath), Condition: "original", File: rep.InputPath,
			F0Median: rep.Input.F0MedianHz, RMSDB: rep.Input.RMSDBFS, PeakDB: rep.Input.PeakDBFS})
	}
	if rep.Reference != nil {
		rows = append(rows, blindRow{ID: blindID("ref", rep.Reference.Path), Condition: "reference", File: rep.Reference.Path,
			F0Median: rep.Reference.Stats.F0MedianHz, RMSDB: rep.Reference.Stats.RMSDBFS, PeakDB: rep.Reference.Stats.PeakDBFS})
	}
	for _, o := range rep.Outputs {
		cond := o.Name
		if o.PresetID != "" {
			cond = "preset:" + o.PresetID
		}
		rows = append(rows, blindRow{ID: blindID(o.Name, o.WavPath), Condition: cond, File: o.WavPath,
			F0Median: o.Stats.F0MedianHz, RMSDB: o.Stats.RMSDBFS, PeakDB: o.Stats.PeakDBFS, RTF: o.RTF})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func writeBlindCSV(path string, rep InputReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create blind dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create blind CSV: %w", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"blind_id", "condition", "wav_path", "f0_median_hz", "rms_dbfs", "peak_dbfs", "rtf"}); err != nil {
		return err
	}
	for _, r := range blindRows(rep) {
		if err := w.Write([]string{r.ID, r.Condition, r.File, f6(r.F0Median), f6(r.RMSDB), f6(r.PeakDB), f6(r.RTF)}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func writeBlindHTML(path string, rep InputReport) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create blind dir: %w", err)
	}
	rows := blindRows(rep)
	display := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.File == "" {
			continue
		}
		// Copying to opaque per-row WAV names keeps the listener-facing page
		// self-contained and prevents preset IDs leaking through file names.
		name := "sample-" + strings.ToLower(r.ID) + ".wav"
		dst := filepath.Join(dir, name)
		b, err := os.ReadFile(r.File)
		if err != nil {
			return fmt.Errorf("read blind WAV %s: %w", r.File, err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return fmt.Errorf("write blind WAV %s: %w", dst, err)
		}
		display[r.ID] = name
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><title>VoiceMorph blind A/B</title>
<style>
body{font-family:system-ui,-apple-system,Segoe UI,sans-serif;margin:32px;line-height:1.45;background:#fafafa;color:#222}
main{max-width:980px;margin:auto}.card{background:white;border:1px solid #ddd;border-radius:12px;padding:16px;margin:12px 0;box-shadow:0 1px 3px #0001}
audio{width:100%;margin-top:10px}.muted{color:#666;font-size:.92em}code{background:#eee;padding:2px 5px;border-radius:4px}
details{margin-top:8px}table{border-collapse:collapse;margin-top:8px}td,th{border-bottom:1px solid #ddd;padding:4px 8px;text-align:left}
</style><main><h1>VoiceMorph 离线盲听 A/B</h1>
<p class="muted">本页由 <code>voice-eval</code> 生成。音频标签只显示盲听 ID，音频副本为随机命名；展开底部映射表后可看到条件。请优先评价自然度、音高目标感和失真。</p>`)
	for i, r := range rows {
		fmt.Fprintf(&b, `<section class="card"><h2>%s</h2><div class="muted">样本 %d</div><audio controls preload="none" src="%s"></audio></section>`,
			html.EscapeString(r.ID), i+1, html.EscapeString(display[r.ID]))
	}
	b.WriteString(`<details><summary>查看盲听映射与指标</summary><table><tr><th>ID</th><th>条件</th><th>文件</th><th>F0中位数</th><th>RMS dBFS</th><th>峰值 dBFS</th><th>RTF</th></tr>`)
	for _, r := range rows {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%.1f</td><td>%.1f</td><td>%.1f</td><td>%.3f</td></tr>`,
			html.EscapeString(r.ID), html.EscapeString(r.Condition), html.EscapeString(r.File), r.F0Median, r.RMSDB, r.PeakDB, r.RTF)
	}
	b.WriteString(`</table></details></main>`)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func blindID(seed, path string) string {
	h := sha256.Sum256([]byte(seed + "\x00" + path))
	return "S" + strings.ToUpper(hex.EncodeToString(h[:4]))
}

var nonName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeName(s string) string {
	s = nonName.ReplaceAllString(strings.TrimSpace(s), "-")
	s = strings.Trim(s, "-._")
	if s == "" {
		return "output"
	}
	return s
}

func f6(v float64) string {
	if v <= -1e100 {
		return "-inf"
	}
	return fmt.Sprintf("%.6f", v)
}
