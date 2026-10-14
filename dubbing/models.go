package dubbing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Model discovery & validation. Verified by inspecting directory
// contents, never by trusting directory names. Order:
// explicit config -> env vars -> ModelScope/HF standard caches ->
// (GUI) manual pick -> (explicit user command only) download.
// Nothing in the analyze path ever downloads.

type ModelCandidate struct {
	Dir    string   `json:"dir"`
	Layout string   `json:"layout"` // "modelscope" | "hf" | "plain"
	Kind   string   `json:"kind"`   // "sensevoice" | "fsmn-vad" | "unknown"
	Valid  bool     `json:"valid"`
	Detail string   `json:"detail"` // evidence files found, or what's missing
}

type ModelStatus struct {
	State       string           `json:"state"` // "ready"|"missing"|"invalid"|"untested"
	ModelDir    string           `json:"model_dir,omitempty"`
	VADDir      string           `json:"vad_dir,omitempty"`
	Source      string           `json:"source"` // "config"|"env"|"cache"|"none"
	Backend     string           `json:"backend"`
	Detail      string           `json:"detail,omitempty"`
	Candidates  []ModelCandidate `json:"candidates,omitempty"`
}

var senseVoiceWeights = []string{"model.pt", "model.safetensors", "model.bin"}
var senseVoiceMeta = []string{"config.yaml", "config.json", "tokens.txt", "chn_jpn_yue_eng_ko_spectro.bpe.model"}

// ValidateModelDir inspects a directory's contents for loadable model
// files for the requested kind ("sensevoice" or "fsmn-vad").
func ValidateModelDir(dir, kind string) ModelCandidate {
	c := ModelCandidate{Dir: dir, Kind: kind, Layout: "plain"}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		c.Detail = "目录不存在"
		return c
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		c.Detail = "不可读: " + err.Error()
		return c
	}
	names := map[string]int64{}
	for _, e := range entries {
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				names[e.Name()] = fi.Size()
			}
		}
	}
	var found []string
	hasWeight := false
	for _, w := range senseVoiceWeights {
		if sz, ok := names[w]; ok {
			if w == "model.pt" && sz < 50_000_000 {
				// fsmn-vad model.pt is small; sensevoice's is ~900MB.
				if kind == "sensevoice" {
					continue
				}
			}
			hasWeight = true
			found = append(found, fmt.Sprintf("%s(%dMB)", w, sz>>20))
		}
	}
	hasMeta := false
	for _, m := range senseVoiceMeta {
		if _, ok := names[m]; ok {
			hasMeta = true
			found = append(found, m)
		}
	}
	if kind == "fsmn-vad" {
		// VAD models are small; weight + config.yaml is enough.
		c.Valid = hasWeight && hasMeta
	} else {
		c.Valid = hasWeight && hasMeta
	}
	if c.Valid {
		c.Detail = "命中: " + strings.Join(found, ", ")
	} else {
		c.Detail = "缺文件（需要权重+配置/词表）；仅找到: " + strings.Join(found, ", ")
		if len(found) == 0 {
			c.Detail = "缺文件（需要权重+配置/词表）；目录内无可识别模型文件"
		}
	}
	return c
}

func hfSnapshotDirs(cache, modelID string) []string {
	// HF cache layout: <cache>/models--iic--SenseVoiceSmall/snapshots/<rev>/
	base := filepath.Join(cache, "models--"+strings.ReplaceAll(modelID, "/", "--"), "snapshots")
	revs, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range revs {
		if r.IsDir() {
			out = append(out, filepath.Join(base, r.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func msLayoutDirs(cache, modelID string) []string {
	// ModelScope cache: <cache>/models/<org>/<name> and legacy <cache>/<org>/<name>
	var out []string
	for _, root := range []string{filepath.Join(cache, "models"), cache} {
		p := filepath.Join(root, modelID)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// ScanModels enumerates every plausible model directory from env +
// standard caches + explicit hints, validating each by content.
func ScanModels(cfg *UserConfig) []ModelCandidate {
	var dirs []struct {
		dir, layout string
	}
	add := func(d, layout string) {
		if d != "" {
			dirs = append(dirs, struct{ dir, layout string }{d, layout})
		}
	}
	if cfg != nil {
		add(cfg.ModelDir, "config")
		add(cfg.VADDir, "config")
	}
	for _, v := range []string{"VOICEMORPH_SENSEVOICE_DIR", "SENSEVOICE_MODEL_DIR", "VOICEMORPH_VAD_DIR"} {
		add(os.Getenv(v), "env")
	}
	// Explicit cache overrides via env.
	for _, c := range []string{os.Getenv("MODELSCOPE_CACHE"), os.Getenv("XDG_CACHE_HOME") + "/modelscope/hub"} {
		for _, id := range []string{"iic/SenseVoiceSmall", "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"} {
			for _, d := range msLayoutDirs(c, id) {
				add(d, "modelscope")
			}
		}
	}
	for _, c := range []string{os.Getenv("HF_HOME"), os.Getenv("HUGGINGFACE_HUB_CACHE"), os.Getenv("XDG_CACHE_HOME") + "/huggingface/hub"} {
		for _, id := range []string{"iic/SenseVoiceSmall", "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"} {
			for _, d := range hfSnapshotDirs(c, id) {
				add(d, "hf")
			}
		}
	}
	// Standard user caches.
	if home, err := os.UserHomeDir(); err == nil {
		for _, base := range []string{
			filepath.Join(home, ".cache/modelscope/hub"),
			filepath.Join(home, ".cache/modelscope"),
		} {
			for _, id := range []string{"iic/SenseVoiceSmall", "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"} {
				for _, d := range msLayoutDirs(base, id) {
					add(d, "modelscope")
				}
			}
		}
		for _, base := range []string{filepath.Join(home, ".cache/huggingface/hub")} {
			for _, id := range []string{"iic/SenseVoiceSmall", "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"} {
				for _, d := range hfSnapshotDirs(base, id) {
					add(d, "hf")
				}
			}
		}
	}
	seen := map[string]bool{}
	var out []ModelCandidate
	for _, d := range dirs {
		if !seen[d.dir] {
			seen[d.dir] = true
			c := ValidateModelDir(d.dir, "sensevoice")
			c.Layout = d.layout
			// VAD dirs validate the same way but get a different kind tag.
			if strings.Contains(d.dir, "vad") || strings.Contains(d.dir, "fsmn_vad") || strings.Contains(d.dir, "fsmn-vad") {
				c.Kind = "fsmn-vad"
			}
			out = append(out, c)
		}
	}
	return out
}

// ResolveModel picks the model dir for analyze: explicit flag >
// config > env > first valid scanned sensevoice dir. VAD dir follows
// the same order restricted to kind fsmn-vad.
func ResolveModel(cfg *UserConfig, explicitDir string) (*ModelStatus, error) {
	try := func(dir, source string) (*ModelStatus, bool) {
		if dir == "" {
			return nil, false
		}
		nd, err := NormalizePath(dir)
		if err != nil {
			return nil, false
		}
		c := ValidateModelDir(nd, "sensevoice")
		if !c.Valid {
			return nil, false
		}
		return &ModelStatus{State: "untested", ModelDir: nd, Source: source, Backend: "funasr", Detail: c.Detail}, true
	}
	if s, ok := try(explicitDir, "flag"); ok {
		resolveVAD(cfg, s)
		return s, nil
	}
	if s, ok := try(cfg.ModelDir, "config"); ok {
		resolveVAD(cfg, s)
		return s, nil
	}
	if s, ok := try(os.Getenv("VOICEMORPH_SENSEVOICE_DIR"), "env"); ok {
		resolveVAD(cfg, s)
		return s, nil
	}
	if s, ok := try(os.Getenv("SENSEVOICE_MODEL_DIR"), "env"); ok {
		resolveVAD(cfg, s)
		return s, nil
	}
	for _, c := range ScanModels(cfg) {
		if c.Valid && c.Kind == "sensevoice" {
			s := &ModelStatus{State: "untested", ModelDir: c.Dir, Source: "cache", Backend: "funasr", Detail: c.Detail}
			resolveVAD(cfg, s)
			return s, nil
		}
	}
	return &ModelStatus{State: "missing", Source: "none", Backend: "funasr",
		Detail: "已扫描 config/env/modelscope/hf 缓存；用 models scan 看证据，models set 指定目录，或 models download"}, nil
}

func resolveVAD(cfg *UserConfig, s *ModelStatus) {
	validVAD := func(dir string) bool {
		nd, err := NormalizePath(dir)
		if err != nil {
			return false
		}
		if ValidateModelDir(nd, "fsmn-vad").Valid {
			s.VADDir = nd
			return true
		}
		return false
	}
	if validVAD(cfg.VADDir) || validVAD(os.Getenv("VOICEMORPH_VAD_DIR")) {
		return
	}
	for _, c := range ScanModels(cfg) {
		if c.Valid && c.Kind == "fsmn-vad" {
			s.VADDir = c.Dir
			return
		}
	}
}

func (s *ModelStatus) MarshalJSON() ([]byte, error) {
	type alias ModelStatus
	return json.Marshal((*alias)(s))
}
