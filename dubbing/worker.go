package dubbing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// WorkerClient talks to the long-lived Python ASR worker over
// stdio JSON-lines: one JSON object per line, requests in, typed
// replies out. Worker stdout carries only protocol messages; all
// worker logs go to its stderr (piped through to ours).

type HelloReply struct {
	WorkerVersion  int      `json:"worker_version"`
	Python         string   `json:"python"`
	FunASR         string   `json:"funasr"`
	Torch          string   `json:"torch"`
	Backends       []string `json:"backends"`
	FunASRImported bool     `json:"funasr_imported"`
}

type LoadedReply struct {
	ModelDir           string `json:"model_dir"`
	VAD                bool   `json:"vad"`
	WordTimestamps     bool   `json:"word_timestamps"`
	SentenceTimestamps bool   `json:"sentence_timestamps"`
	Detail             string `json:"detail"`
}

type ProgressEvent struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type workerMsg struct {
	Type    string          `json:"type"`
	Cmd     string          `json:"cmd,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Fatal   bool            `json:"fatal,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

type WorkerClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan lineResult
	closed bool
}

type lineResult struct {
	raw []byte
	err error
}

func WorkerScriptPath() (string, error) {
	if p := os.Getenv("VOICEMORPH_WORKER"); p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		for _, p := range []string{
			filepath.Join(filepath.Dir(exe), "resources/asr/dub_worker.py"),
			filepath.Join(filepath.Dir(exe), "asr/dub_worker.py"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	// Source-tree layout: <repo>/asr/dub_worker.py relative to CWD
	// or to this file's package dir.
	for _, p := range []string{
		"asr/dub_worker.py",
		"../asr/dub_worker.py",
	} {
		if _, err := os.Stat(p); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				return abs, nil
			}
		}
	}
	return "", fmt.Errorf("找不到 asr/dub_worker.py（设 VOICEMORPH_WORKER 或在仓库根运行）")
}

// FindPython resolves the interpreter: config > env > PATH.
func FindPython(cfg *UserConfig) (string, error) {
	var cands []string
	if cfg != nil && cfg.PythonPath != "" {
		cands = append(cands, cfg.PythonPath)
	}
	if v := os.Getenv("VOICEMORPH_PYTHON"); v != "" {
		cands = append(cands, v)
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			cands = append(cands, p)
		}
	}
	for _, p := range cands {
		out, err := exec.Command(p, "-c", "import sys;print('%d.%d'%sys.version_info[:2])").Output()
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(out))
		var maj, min int
		if n, _ := fmt.Sscanf(v, "%d.%d", &maj, &min); n == 2 && (maj > 3 || (maj == 3 && min >= 9)) {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 python>=3.9 解释器（config.python_path / VOICEMORPH_PYTHON / PATH）")
}

type SpawnOptions struct {
	Python  string
	Script  string
	Offline bool
	ExtraEnv []string
}

func SpawnWorker(opts SpawnOptions) (*WorkerClient, error) {
	if opts.Python == "" || opts.Script == "" {
		return nil, fmt.Errorf("SpawnOptions requires Python and Script")
	}
	cmd := exec.Command(opts.Python, "-u", opts.Script)
	cmd.Stderr = os.Stderr
	env := append([]string{}, os.Environ()...)
	if opts.Offline {
		env = append(env,
			"HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1",
			"MODELSCOPE_OFFLINE=1", "TOKENIZERS_PARALLELISM=false")
	}
	env = append(env, opts.ExtraEnv...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &WorkerClient{cmd: cmd, stdin: stdin, lines: make(chan lineResult, 64)}
	go func() {
		defer close(w.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			b := append([]byte(nil), sc.Bytes()...)
			w.lines <- lineResult{raw: b}
		}
		w.lines <- lineResult{err: io.EOF}
	}()
	return w, nil
}

func (w *WorkerClient) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.stdin.Write(append(b, '\n'))
	return err
}

// recv reads until a message matching want arrives; progress events
// are relayed to onProgress; error messages become errors.
func (w *WorkerClient) recv(want string, onProgress func(ProgressEvent)) (json.RawMessage, *workerMsg, error) {
	for r := range w.lines {
		if r.err != nil {
			return nil, nil, fmt.Errorf("worker exited: %w", r.err)
		}
		var m workerMsg
		if err := json.Unmarshal(r.raw, &m); err != nil {
			continue // non-JSON noise on stdout; ignore
		}
		switch m.Type {
		case "progress":
			if onProgress != nil {
				var p ProgressEvent
				if json.Unmarshal(r.raw, &p) == nil {
					onProgress(p)
				}
			}
		case "error":
			return nil, &m, fmt.Errorf("worker %s: %s", m.Code, m.Message)
		default:
			if m.Type == want || want == "" {
				return r.raw, &m, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("worker exited without reply")
}

func (w *WorkerClient) Hello() (*HelloReply, error) {
	if err := w.send(map[string]string{"cmd": "hello"}); err != nil {
		return nil, err
	}
	raw, _, err := w.recv("hello", nil)
	if err != nil {
		return nil, err
	}
	var h HelloReply
	return &h, json.Unmarshal(raw, &h)
}

func (w *WorkerClient) LoadModel(modelDir, vadDir, device, language string, ncpu int) (*LoadedReply, error) {
	req := map[string]any{
		"cmd": "load", "model_dir": modelDir, "device": device,
		"language": language, "ncpu": ncpu,
	}
	if vadDir != "" {
		req["vad_dir"] = vadDir
	}
	if err := w.send(req); err != nil {
		return nil, err
	}
	raw, _, err := w.recv("loaded", nil)
	if err != nil {
		return nil, err
	}
	var r LoadedReply
	return &r, json.Unmarshal(raw, &r)
}

// TranscribeResult is the worker's normalized payload; segments land
// verbatim in asr-raw.json so raw ASR evidence is never discarded.
type TranscribeResult struct {
	Segments []ASRSegment    `json:"segments"`
	Backend  json.RawMessage `json:"backend"`
}

func (w *WorkerClient) Transcribe(audio string, offsetMs int64, language string, wantWords, mergeVAD bool, mergeLenS, batchSizeS int, useITN bool, onProgress func(ProgressEvent)) (*TranscribeResult, error) {
	req := map[string]any{
		"cmd": "transcribe", "audio": audio, "offset_ms": offsetMs,
		"language": language, "want_words": wantWords,
		"merge_vad": mergeVAD, "merge_length_s": mergeLenS,
		"batch_size_s": batchSizeS, "use_itn": useITN,
	}
	if err := w.send(req); err != nil {
		return nil, err
	}
	raw, m, err := w.recv("result", onProgress)
	if err != nil {
		return nil, err
	}
	var t TranscribeResult
	if m.Payload != nil {
		if err := json.Unmarshal(m.Payload, &t); err != nil {
			return nil, fmt.Errorf("bad worker payload: %w", err)
		}
		return &t, nil
	}
	_ = raw
	return &t, nil
}

// Download is the ONLY network-capable worker call — the CLI gates it
// behind `models download --yes`; analyze/load never reach it.
func (w *WorkerClient) Download(kind, dest string) error {
	if err := w.send(map[string]any{"cmd": "download", "kind": kind, "dest": dest}); err != nil {
		return err
	}
	_, _, err := w.recv("downloaded", nil)
	return err
}

func (w *WorkerClient) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.send(map[string]string{"cmd": "shutdown"})
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = w.cmd.Process.Kill()
		<-done
	}
	return nil
}
