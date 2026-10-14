// voice-dub — offline multi-character dubbing pipeline CLI.
//
// Agent-first interface: every command prints exactly one JSON object
// to stdout; progress/logs go to stderr. Exit codes: 0 ok, 1 failed
// (error.code in JSON), 2 usage error. Nothing in this tool ever
// triggers a network download except `models download --yes`.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"voice-morph/dubbing"
)

const toolVersion = "voice-dub 1.0.0"

type out struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
	Err     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  string `json:"detail,omitempty"`
	} `json:"error,omitempty"`
	Extra map[string]any `json:"-"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func emitJSON(w io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(w, string(b))
}

func fail(command, code string, err error) int {
	var o out
	o.OK = false
	o.Command = command
	o.Err = &struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  string `json:"detail,omitempty"`
	}{Code: code, Message: err.Error()}
	emitJSON(os.Stdout, o)
	if code == "E_USAGE" {
		return 2
	}
	return 1
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		emitJSON(stdout, map[string]any{"ok": true, "command": "version", "tool": toolVersion})
		return 0
	case "models":
		if len(args) < 2 {
			usage(stderr)
			return 2
		}
		return runModels(args[1:], stdout, stderr)
	case "analyze":
		return runAnalyze(args[1:], stdout, stderr)
	case "reconcile":
		return runReconcile(args[1:], stdout, stderr)
	case "render":
		return runRender(args[1:], stdout, stderr)
	default:
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `voice-dub — 离线多角色配音流水线

voice-dub <command> [flags]

  models scan|status|set|test|download   模型发现/校验/下载
  analyze    --audio WAV --script script.json --out DIR   ASR+校对
  reconcile  --out DIR [--script P --asr P] [--merge-reviewed]  重跑校对
  render     --timeline P --voices voices.json --out DIR [--audio A --only ids --src --keep-going --jobs N]  批量变声导出
  version`)
}

func cfgPath(fs *flag.FlagSet) *string {
	return fs.String("config", "", "用户配置 dubbing.json 路径（测试用）")
}

func progressLog(stderr io.Writer) func(dubbing.ProgressEvent) {
	return func(p dubbing.ProgressEvent) {
		fmt.Fprintf(stderr, "\r转写 %d/%d", p.Done, p.Total)
	}
}

func runModels(args []string, stdout, stderr io.Writer) int {
	sub := args[0]
	fs := flag.NewFlagSet("models "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cp := cfgPath(fs)
	var dir, vadDir, dest, kind string
	var yes bool
	switch sub {
	case "set":
		fs.StringVar(&dir, "dir", "", "SenseVoiceSmall 模型目录（必填）")
		fs.StringVar(&vadDir, "vad-dir", "", "fsmn-vad 模型目录")
	case "test":
		fs.StringVar(&dir, "dir", "", "覆盖模型目录")
	case "download":
		fs.StringVar(&dest, "dest", "", "下载目标目录（必填）")
		fs.StringVar(&kind, "kind", "sensevoice", "sensevoice|fsmn-vad")
		fs.BoolVar(&yes, "yes", false, "确认联网下载（非交互必需）")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := dubbing.LoadUserConfig(*cp)
	if err != nil {
		return fail("models "+sub, "E_IO", err)
	}
	switch sub {
	case "scan":
		emitJSON(stdout, map[string]any{"ok": true, "command": "models scan",
			"candidates": dubbing.ScanModels(cfg)})
		return 0
	case "status":
		ms, _ := dubbing.ResolveModel(cfg, "")
		emitJSON(stdout, ms)
		if ms.State == "missing" {
			return 1
		}
		return 0
	case "set":
		if dir == "" {
			return fail("models set", "E_USAGE", fmt.Errorf("--dir 必填"))
		}
		nd, err := dubbing.NormalizePath(dir)
		if err != nil {
			return fail("models set", "E_MODEL_INVALID", err)
		}
		c := dubbing.ValidateModelDir(nd, "sensevoice")
		if !c.Valid {
			return fail("models set", "E_MODEL_INVALID", fmt.Errorf("%s", c.Detail))
		}
		cfg.ModelDir = nd
		if vadDir != "" {
			nv, err := dubbing.NormalizePath(vadDir)
			if err != nil {
				return fail("models set", "E_MODEL_INVALID", err)
			}
			cfg.VADDir = nv
		}
		if err := cfg.Save(*cp); err != nil {
			return fail("models set", "E_IO", err)
		}
		emitJSON(stdout, map[string]any{"ok": true, "command": "models set",
			"model_dir": nd, "vad_dir": cfg.VADDir, "evidence": c.Detail})
		return 0
	case "test":
		ms, err := dubbing.ResolveModel(cfg, dir)
		if err != nil {
			return fail("models test", "E_MODEL_NOT_FOUND", err)
		}
		if ms.State == "missing" {
			emitJSON(stdout, ms)
			return 1
		}
		py, err := dubbing.FindPython(cfg)
		if err != nil {
			return fail("models test", "E_WORKER_SPAWN", err)
		}
		wp, err := dubbing.WorkerScriptPath()
		if err != nil {
			return fail("models test", "E_WORKER_SPAWN", err)
		}
		w, err := dubbing.SpawnWorker(dubbing.SpawnOptions{Python: py, Script: wp, Offline: true})
		if err != nil {
			return fail("models test", "E_WORKER_SPAWN", err)
		}
		defer w.Close()
		hello, err := w.Hello()
		if err != nil {
			return fail("models test", "E_WORKER_FAIL", err)
		}
		lr, err := w.LoadModel(ms.ModelDir, ms.VADDir, "cpu", cfg.Language, 0)
		if err != nil {
			return fail("models test", "E_MODEL_INVALID", err)
		}
		emitJSON(stdout, map[string]any{"ok": true, "command": "models test",
			"hello": hello, "loaded": lr})
		return 0
	case "download":
		if dest == "" {
			return fail("models download", "E_USAGE", fmt.Errorf("--dest 必填"))
		}
		if !yes {
			return fail("models download", "E_USAGE", fmt.Errorf(
				"将联网下载 %s 到 %s；确认请加 --yes", kind, dest))
		}
		py, err := dubbing.FindPython(cfg)
		if err != nil {
			return fail("models download", "E_WORKER_SPAWN", err)
		}
		wp, err := dubbing.WorkerScriptPath()
		if err != nil {
			return fail("models download", "E_WORKER_SPAWN", err)
		}
		w, err := dubbing.SpawnWorker(dubbing.SpawnOptions{Python: py, Script: wp, Offline: false})
		if err != nil {
			return fail("models download", "E_WORKER_SPAWN", err)
		}
		defer w.Close()
		if err := w.Download(kind, dest); err != nil {
			return fail("models download", "E_WORKER_FAIL", err)
		}
		emitJSON(stdout, map[string]any{"ok": true, "command": "models download", "kind": kind, "dest": dest})
		return 0
	}
	usage(stderr)
	return 2
}

func runAnalyze(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	audio := fs.String("audio", "", "输入 WAV（必填）")
	script := fs.String("script", "", "台本 JSON（可选；给了就跑校对）")
	outDir := fs.String("out", "", "会话输出目录（必填）")
	modelDir := fs.String("model-dir", "", "覆盖模型目录")
	lang := fs.String("lang", "zh", "语言")
	chunkSec := fs.Int("chunk-sec", 300, "Go 侧兜底分块窗口秒数（0=不预分块）")
	noVAD := fs.Bool("no-vad", false, "禁用 VAD")
	cp := cfgPath(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *audio == "" || *outDir == "" {
		return fail("analyze", "E_USAGE", fmt.Errorf("--audio 与 --out 必填"))
	}
	doc, tl, err := dubbing.Analyze(dubbing.AnalyzeOptions{
		AudioPath: *audio, ScriptPath: *script, OutDir: *outDir,
		ModelDir: *modelDir, Language: *lang, ChunkSec: *chunkSec,
		NoVAD: *noVAD, ConfigPath: *cp, OnProgress: progressLog(stderr)})
	if err != nil {
		code := "E_ASR"
		msg := err.Error()
		for _, p := range []string{"E_MODEL_NOT_FOUND", "E_MODEL_INVALID", "E_WORKER_SPAWN", "E_WORKER_FAIL", "E_SCRIPT_INVALID"} {
			if strings.Contains(msg, p) {
				code = p
			}
		}
		return fail("analyze", code, err)
	}
	r := map[string]any{"ok": true, "command": "analyze", "out_dir": *outDir,
		"asr_raw": filepath.Join(*outDir, "asr-raw.json"), "segments": len(doc.Segments)}
	if tl != nil {
		r["timeline"] = filepath.Join(*outDir, "timeline.json")
		r["review_pending"] = tl.ReviewCount
	}
	emitJSON(stdout, r)
	return 0
}

func runReconcile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out", "", "会话目录（必填）")
	scriptP := fs.String("script", "", "台本路径（默认用会话内已有）")
	asrP := fs.String("asr", "", "asr-raw 路径（默认 out/asr-raw.json）")
	merge := fs.Bool("merge-reviewed", false, "合并 timeline-reviewed.json 的人工编辑")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outDir == "" {
		return fail("reconcile", "E_USAGE", fmt.Errorf("--out 必填"))
	}
	if *asrP == "" {
		*asrP = filepath.Join(*outDir, "asr-raw.json")
	}
	doc, err := dubbing.LoadASRDoc(*asrP)
	if err != nil {
		return fail("reconcile", "E_ASR", err)
	}
	var sc *dubbing.Script
	if *scriptP != "" {
		if sc, err = dubbing.LoadScript(*scriptP); err != nil {
			return fail("reconcile", "E_SCRIPT_INVALID", err)
		}
	} else {
		old, err := dubbing.LoadTimeline(filepath.Join(*outDir, "timeline.json"))
		if err == nil && old.Script != "" {
			sc, _ = dubbing.LoadScript(old.Script)
		}
		if sc == nil {
			return fail("reconcile", "E_USAGE", fmt.Errorf("找不到台本；请传 --script"))
		}
	}
	tl, err := dubbing.Reconcile(sc, doc, dubbing.DefaultReconcileOptions())
	if err != nil {
		return fail("reconcile", "E_ASR", err)
	}
	tl.Audio = doc.Audio
	tl.AudioSHA256 = doc.AudioSHA256
	tl.SampleRate = doc.SampleRate
	tl.DurationMs = doc.DurationMs
	tl.Script = scPath(sc, *scriptP, *outDir)
	tl.ASRFile = *asrP
	if *merge {
		if old, err := dubbing.LoadTimeline(filepath.Join(*outDir, "timeline-reviewed.json")); err == nil {
			tl = dubbing.MergeReviewed(tl, old)
		}
	}
	path := filepath.Join(*outDir, "timeline.json")
	if err := dubbing.SaveJSON(path, tl); err != nil {
		return fail("reconcile", "E_IO", err)
	}
	emitJSON(stdout, map[string]any{"ok": true, "command": "reconcile",
		"timeline": path, "segments": len(tl.Segments), "review_pending": tl.ReviewCount})
	return 0
}

func scPath(sc *dubbing.Script, explicit, outDir string) string {
	if explicit != "" {
		return explicit
	}
	old, err := dubbing.LoadTimeline(filepath.Join(outDir, "timeline.json"))
	if err == nil {
		return old.Script
	}
	return ""
}

func runRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tlP := fs.String("timeline", "", "timeline(-reviewed).json（必填）")
	voicesP := fs.String("voices", "", "voices.json（必填）")
	outDir := fs.String("out", "", "输出目录（必填，建议 <session>/rendered）")
	only := fs.String("only", "", "逗号分隔的 segment_id/line_id 白名单")
	audioP := fs.String("audio", "", "覆盖 timeline.audio 的音频路径（仍校验 sha256）")
	src := fs.Bool("src", false, "每段附带原声切片 lines/src-<seg>.wav")
	keep := fs.Bool("keep-going", false, "单段失败继续")
	jobs := fs.Int("jobs", 0, "并行渲染数")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tlP == "" || *voicesP == "" || *outDir == "" {
		return fail("render", "E_USAGE", fmt.Errorf("--timeline --voices --out 必填"))
	}
	var onlySet map[string]bool
	if *only != "" {
		onlySet = map[string]bool{}
		for _, s := range strings.Split(*only, ",") {
			onlySet[strings.TrimSpace(s)] = true
		}
	}
	m, err := dubbing.Render(dubbing.RenderOptions{
		TimelinePath: *tlP, VoicesPath: *voicesP, AudioPath: *audioP, OutDir: *outDir,
		Only: onlySet, KeepGoing: *keep, Jobs: *jobs, WriteSrc: *src,
		OnProgress: func(d, t int) { fmt.Fprintf(stderr, "\r渲染 %d/%d", d, t) }})
	if err != nil && m == nil {
		code := "E_IO"
		if strings.Contains(err.Error(), "timeline") {
			code = "E_TIMELINE_INVALID"
		}
		if strings.Contains(err.Error(), "voices") {
			code = "E_VOICES_INVALID"
		}
		return fail("render", code, err)
	}
	r := map[string]any{"ok": m.Status == "ok", "command": "render",
		"manifest": filepath.Join(*outDir, "manifest.json"),
		"lines": len(m.Lines), "skipped": len(m.Skipped), "failures": len(m.Failures),
		"mix": m.Mix, "roles": m.Roles, "subtitles": m.Subtitles, "warnings": m.Warnings}
	emitJSON(stdout, r)
	if m.Status != "ok" {
		return 1
	}
	return 0
}
