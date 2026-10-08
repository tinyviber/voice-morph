package engine

// Preset is a named voice (MorphVOX calls them voice aliases). Pitch and
// timbre values follow the examples in the MorphVOX Pro docs.
type Preset struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	NameZh string `json:"nameZh"`
	Hint   string `json:"hint"` // one-line description shown in the UI
	Params Params `json:"params"`
}

// Presets: (pitch, timbre) pairs mirror the documented MorphVOX examples —
// (0,0)=normal, (-0.5,0)=low, (-0.5,-0.3)=giant, (+0.8,0)=high,
// (+0.8,+0.4)=child — extended to a full set of aliases.
var Presets = []Preset{
	{ID: "normal", Name: "Normal", NameZh: "原声", Hint: "不处理",
		Params: Params{Pitch: 0, Timbre: 0, Strength: 1, Gain: 1}},
	{ID: "low", Name: "Low", NameZh: "低沉", Hint: "低半档音高",
		Params: Params{Pitch: -0.5, Timbre: 0, Strength: 1, Gain: 1}},
	{ID: "giant", Name: "Giant", NameZh: "巨人", Hint: "又低又宽",
		Params: Params{Pitch: -0.5, Timbre: -0.3, Strength: 1, Gain: 1}},
	{ID: "demon", Name: "Demon", NameZh: "恶魔", Hint: "极低共振峰",
		Params: Params{Pitch: -0.8, Timbre: -0.5, Strength: 1, Gain: 1}},
	{ID: "woman", Name: "Woman", NameZh: "女声", Hint: "偏高偏细",
		Params: Params{Pitch: 0.45, Timbre: 0.28, Strength: 1, Gain: 1}},
	{ID: "high", Name: "High", NameZh: "高音", Hint: "纯音高上移",
		Params: Params{Pitch: 0.8, Timbre: 0, Strength: 1, Gain: 1}},
	{ID: "child", Name: "Child", NameZh: "小孩", Hint: "高音高共振峰",
		Params: Params{Pitch: 0.8, Timbre: 0.4, Strength: 1, Gain: 1}},
	{ID: "chipmunk", Name: "Chipmunk", NameZh: "花栗鼠", Hint: "极高玩梗",
		Params: Params{Pitch: 1, Timbre: 0.55, Strength: 1, Gain: 1}},
}
