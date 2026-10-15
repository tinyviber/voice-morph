package dubbing

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// stub worker: answers the JSON-lines protocol without funasr.
const stubPy = `import json,sys
for line in sys.stdin:
    r=json.loads(line)
    c=r.get("cmd")
    if c=="hello":
        print(json.dumps({"type":"hello","worker_version":1,"python":"3.x","funasr":"","torch":"","backends":[],"funasr_imported":False}),flush=True)
    elif c=="load":
        print(json.dumps({"type":"loaded","model_dir":r["model_dir"],"vad":False,"word_timestamps":False,"sentence_timestamps":True,"detail":"stub"}),flush=True)
    elif c=="transcribe":
        print(json.dumps({"type":"progress","done":1,"total":1}),flush=True)
        print(json.dumps({"type":"result","payload":{"segments":[{"text":"测试","start_ms":0,"end_ms":500}],"backend":{"funasr_version":"stub"}}}),flush=True)
    elif c=="shutdown":
        print(json.dumps({"type":"bye"}),flush=True);break
`

func spawnStub(t *testing.T) *WorkerClient {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script := filepath.Join(t.TempDir(), "stub.py")
	if err := os.WriteFile(script, []byte(stubPy), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := SpawnWorker(SpawnOptions{Python: py, Script: script, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorkerProtocol(t *testing.T) {
	w := spawnStub(t)
	defer w.Close()
	h, err := w.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if h.WorkerVersion != 1 {
		t.Fatalf("worker_version=%d", h.WorkerVersion)
	}
	lr, err := w.LoadModel("/tmp/nonexistent-model", "", "cpu", "zh", 0)
	if err != nil {
		t.Fatal(err)
	}
	if lr.SentenceTimestamps != true {
		t.Fatal("loaded reply mismatch")
	}
	var prog []ProgressEvent
	r, err := w.Transcribe("/tmp/x.wav", 0, "zh", false, true, 15, 300, true, 4000,
		func(p ProgressEvent) { prog = append(prog, p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Segments) != 1 || r.Segments[0].Text != "测试" {
		t.Fatalf("segments=%+v", r.Segments)
	}
	if len(prog) == 0 {
		t.Fatal("no progress event")
	}
}

func TestValidateModelDir(t *testing.T) {
	dir := t.TempDir()
	c := ValidateModelDir(dir, "sensevoice")
	if c.Valid {
		t.Fatal("empty dir should not validate")
	}
	big := make([]byte, 60<<20)
	if err := os.WriteFile(filepath.Join(dir, "model.pt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	c = ValidateModelDir(dir, "sensevoice")
	if c.Valid {
		t.Fatal("weight without config should not validate")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("a: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = ValidateModelDir(dir, "sensevoice")
	if !c.Valid {
		t.Fatalf("should validate: %s", c.Detail)
	}
}

func TestResolveModelMissing(t *testing.T) {
	t.Setenv("VOICEMORPH_SENSEVOICE_DIR", "")
	t.Setenv("SENSEVOICE_MODEL_DIR", "")
	t.Setenv("VOICEMORPH_VAD_DIR", "")
	ms, err := ResolveModel(&UserConfig{Version: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ms.State != "missing" {
		t.Fatalf("state=%s", ms.State)
	}
}
