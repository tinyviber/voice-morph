# voice-dub CLI 使用手册（agent 可调）

构建：`go build -o voice-dub ./cmd/voice-dub`

**约定**：每条命令向 stdout 输出恰好一个 JSON 对象；进度写 stderr。
退出码 `0` 成功 / `1` 执行失败（JSON 里 `error.code` 给原因）/ `2` 用法错误。
除 `models download --yes` 外没有任何联网路径；默认全离线。
全局 flag `-config PATH` 覆盖用户配置文件（默认
`<os.UserConfigDir>/voice-morph/dubbing.json`）。

## 命令

### `voice-dub models scan`
列出发现的全部候选模型目录（config/env/ModelScope/HF 缓存），每个带
校验证据（命中的权重/配置文件名），`candidates` 为空数组也是正常输出。

### `voice-dub models status`
当前生效的模型决议：`{state:"ready|missing|invalid|untested",
model_dir, vad_dir, source:"config|env|cache|none", detail}`。
`state:"missing"` 时退出码 1。

### `voice-dub models set --dir DIR [--vad-dir DIR]`
校验目录内容（权重 >50MB + 配置/词表）→ 规范化路径 → 写用户配置。
GUI 与 CLI 共享同一份配置。

### `voice-dub models test [-dir DIR]`
真实拉起 worker：`hello`（上报 python/funasr/torch 版本与
`funasr_imported`）→ `load`（上报 `word_timestamps` 能力探针结果）。
这是最权威的"模型能不能用"答案。

### `voice-dub models download --dest DIR --kind sensevoice|fsmn-vad --yes`
唯一联网路径。非交互必须 `--yes`；不带 `--yes` 打印说明并以 2 退出。

### `voice-dub analyze --audio WAV [--script script.json] --out DIR [--model-dir DIR] [--lang zh] [--chunk-sec 300] [--no-vad]`
跑 ASR 写 `<out>/asr-raw.json`；给了 `--script` 就继续跑校对写
`<out>/timeline.json`，stdout 里 `review_pending` 是待人工审核段数——
它是数据不是错误，agent 据此决定是否先让人审再 render。

### `voice-dub reconcile --out DIR [--script P --asr P] [--merge-reviewed]`
单独重跑校对（不跑 ASR）。`--merge-reviewed` 把
`<out>/timeline-reviewed.json` 里 `edited:true` 的行（角色/文本/边界/
选用）按 (line_id, asr_ref, ±2s) 并回新草稿。

### `voice-dub render --timeline P --voices voices.json --out DIR [--audio WAV] [--only id1,id2] [--src] [--allow-unreviewed] [--keep-going] [-jobs N]`
读审核后的 timeline 批量变声。`--audio` 可覆盖 timeline 里的音频路径
（仍会校验 sha256，换了录音会拒绝）；`--src` 让每行附带原声切片
`lines/src-<segid>.wav` 供 A/B 对比。产物：

```
<out>/
  manifest.json          一切产物的清单（含 skipped/failures/warnings）
  lines/<seg>-<key8>.wav 逐句 WAV（内容寻址：参数或切点变 = 新文件名）
  roles/<role>.wav       每角色完整音轨（与源等长，停顿原位保留）
  mix.wav                全角色混音轨
  subtitles.srt / subtitles.json   字幕（cue 时间=真实渲染切点）
```

`--only` 按 segment_id/line_id 白名单重渲局部；`--keep-going` 单段失败
继续（manifest `status:"failed"` + `failures[]`），否则遇错即停且不写
manifest。

**审核门**：`review_required:true` 且未显式确认（`reviewed:false`）的
选中段默认进 `skipped`（原因 `unreviewed anomaly`）；`--allow-unreviewed`
显式放行但逐段记 `warnings`。只改文本/角色/勾选置 `edited` 不放行——
审核必须走显式确认。

**manifest.status**（下游可机读）：`complete`=所有选中行都已渲染且无
未补录的台本行；`blocked_review`=有选中行仅因待审核被跳过；
`partial`=有缺失——整句漏读（`unmatched_lines` 未标 `optional:true`）
或其他选中行未渲染，**优先级高于 blocked_review**（审核补不回没录的音）；
`failed`=有渲染失败。`manifest.counts` 给结构化计数
（rendered/failures/blocked_review/missing_lines/unselected_takes）。
CLI `ok` 与退出码跟着 status 走（complete→true/0，其余→false/1）。
台本行确属有意不录时，在 timeline-reviewed.json 的 `unmatched_lines`
里给该行加 `optional:true`——渲染不再降级，skipped 仍如实记录。

缓存真实生效：同 seg/切点/音频/参数的行 WAV 已存在且解码长度正确时
直接复用（`cache_hit:true`），参数或切点变化自动失配；缓存键用实际
读到的音频 SHA256，缺声明的时间线也偷不了缓存。文件名安全：
`role_id`/`segment_id` 只取 [A-Za-z0-9\-_.] 再统一追加原 id hash8
（`roles/<slug>-<hash8>.wav`、`lines/src-<slug>-<hash8>.wav`）——
中文名、`hero.`/`hero`、`Narrator`/`narrator`（大小写不敏感文件系统）
都不会互撞，恶意 id 也不可能把产物写出输出目录。

## manifest.json 结构

```jsonc
{
  "version": 1,
  "audio": "take.wav", "audio_sha256": "…", "sample_rate": 48000,
  "duration_ms": 12000, "status": "complete|blocked_review|partial|failed",
  "lines": [{"segment_id":"seg-0001","line_id":"l01","role_id":"narrator",
             "wav":"lines/seg-0001-xxxxxxxx.wav",
             "src_wav":"lines/src-seg-0001.wav",   // --src 时
             "cut_start_ms":480,
             "cut_end_ms":2500,"duration_ms":2020,"cache_hit":false,
             "backend":"dsp","experimental":false}],
  "extra": { /* RenderOptions.Extra 透传槽，下游可写 */ },
  "skipped": [{"id":"seg-0004","reason":"take not selected"},
              {"id":"l04","reason":"script line has no audio"}],
  "failures": [], "roles": {"narrator":"roles/narrator-a1b2c3d4.wav"},
  "counts": {"rendered":3,"failures":0,"blocked_review":0,
             "missing_lines":0,"unselected_takes":1},
  "mix": "mix.wav", "subtitles": "subtitles.srt",
  "subtitle_json": "subtitles.json", "warnings": []
}
```

## timeline.json 结构（校对/审核协议核心）

```jsonc
{
  "version": 1, "audio": "…", "audio_sha256": "…",
  "sample_rate": 48000, "duration_ms": 12000,
  "script": "script.json", "asr_file": "asr-raw.json",
  "segments": [{
    "segment_id": "seg-0001", "asr_segment_id": "seg-0001",
    "line_id": "l01", "role_id": "narrator",
    "script_text": "大家好，我们开始吧",     // 台本原文（参考）
    "asr_raw": "大家好我们开始吧",           // ASR 原文（证据，不删）
    "spoken_text": "大家好我们开始吧",       // 实际朗读（渲染文本=事实）
    "start_ms": 500, "end_ms": 2500,
    "match_status": "exact|modified|repeated|unmatched|manual",
    "similarity": 1.0,
    "review_required": false, "reviewed": false,
    "review_reasons": ["low_similarity","estimated_boundary",
      "ambiguous_takes","unmatched_audio","low_asr_confidence","needs_role"],
    "take_index": 2, "selected": true,       // 重读的候选 take
    "part_index": 1,                          // 一行被停顿拆成多段时的段序
    "boundary": "measured|word|estimated",   // 边界可信度
    "edited": false,                         // 人工碰过 → 重跑校对时被保留
    "reviewed": false                        // 显式确认 → 才解除审核门
  }],
  "warnings": [],                            // 校对级全局警告
  "unmatched_lines": [{"line_id":"l04","role_id":"narrator",
                       "text":"我们下午再练","reason":"no_audio"}],
  "review_count": 4
}
```

**审核写法**：外部 agent 或人直接编辑 `timeline-reviewed.json`
（复制 timeline.json 改名即可）：改 `role_id`/`spoken_text`/
`start_ms`/`end_ms`/`selected` 时把对应段 `edited:true`；确认某段
无问题把 `reviewed:true`（只有它才解除审核门）。unmatched 段想
收编：填 `role_id` + `selected:true` + `edited:true`（→ manual）。

## 典型 agent 流程

```bash
voice-dub models status                    # 1) 模型就位？
voice-dub analyze --audio take.wav --script s.json --out sess1   # 2) 转写+校对
# 3) review_pending>0 → 让人或 agent 改 sess1/timeline.json
#    存为 sess1/timeline-reviewed.json
voice-dub render --timeline sess1/timeline-reviewed.json \
  --voices sess1/voices.json --out sess1/rendered         # 4) 渲染
# 5) manifest.json 交给下游视频 agent：lines[] + roles[] + subtitles
```

断点续跑：asr-raw.json 在就不必重跑 ASR（`reconcile` 单跑）；timeline
在就不必重跑校对（render 只吃 timeline）；逐段 WAV 内容寻址，
`--only` 只重渲变化段。

## 无模型时的离线 demo（确定性，不下载任何东西）

```bash
cd examples/dubbing
go run ./gen                    # 生成 take.wav（合成示例音频）
voice-dub reconcile --out ./out --script script.json --asr asr-raw.json
voice-dub render --timeline out/timeline.json --voices voices.json --out out/rendered
```

`asr-raw.json` 是手写 fixture——覆盖漏读、改写、无关音频、口误重读
四种异常；渲染产物在 `out/rendered/`。真实 ASR 链路需要
SenseVoiceSmall 模型（`models download`）+ `models test` 实测。
