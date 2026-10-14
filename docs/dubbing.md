# 离线多角色配音子系统设计

VoiceMorph 的第二个工作区：**录音是事实来源，台本只是参考**。
朗读者一口气念完所有角色的台词，本系统负责识别实际说了什么、把每段
语音钉回台本行、把人读错/漏读/重读的部分标记出来让人审核，审核后批量
离线渲染成各角色音轨。

```
录音 WAV ──► A. 转写（FunASR/SenseVoiceSmall，本地 Python worker）
                │   asr-raw.json（原始证据，永不改写）
                ▼
           B. 校对（dubbing.Reconcile，纯 Go 确定性 DP）
                │   timeline.json → timeline-reviewed.json（人工编辑层）
                ▼
           C. 渲染（dubbing.Render，engine 批处理）
                rendered/：lines/*.wav、roles/<角色>.wav、mix.wav、
                          subtitles.srt / subtitles.json、manifest.json
```

三层各自由 CLI 子命令独立驱动，产物都是带 `version` 的 JSON——每一层
的输出都是下一层可缓存的输入；重跑 render 不需要重跑 ASR。

## A. 转写（ASR）

- **进程模型**：`asr/dub_worker.py` 常驻子进程，stdio JSON-lines 协议
  （`hello`/`load`/`transcribe`/`download`/`shutdown`）。模型只在 `load`
  加载一次；`transcribe` 复用。Go 侧永远不把 Python/PyTorch/FunASR 链进
  `engine/`——engine 保持纯 Go、零 cgo。
- **离线保证**（三层）：`AutoModel(model=<本地目录>)` 不经过 hub 解析；
  `disable_update=True`；worker 进程注入 `HF_HUB_OFFLINE=1 /
  TRANSFORMERS_OFFLINE=1 / MODELSCOPE_OFFLINE=1`。任何分析路径都不存在
  下载分支；下载只能由 `voice-dub models download --yes` 显式触发。
- **模型发现顺序**：显式配置（`dubbing.json`）→ 环境变量
  （`VOICEMORPH_SENSEVOICE_DIR` / `VOICEMORPH_VAD_DIR` /
  `SENSEVOICE_MODEL_DIR` / `MODELSCOPE_CACHE` / `HF_HOME` /
  `HUGGINGFACE_HUB_CACHE`）→ ModelScope/HF 标准缓存目录（新旧两种
  layout 都探）→ GUI 对话框手选 → 用户确认的下载。校验看内容不看
  名字：权重文件（>50MB）+ 配置/词表任一，证据写进 `models scan` 输出。
- **时间戳能力如实上报**：`output_timestamp` 的字段格式随 funasr 版本
  变化，worker 在 `load` 后跑 1 秒探针检测 `words`/`timestamp` 字段实际
  存在才报 `word_timestamps: true`；否则 Go 侧降级到句段边界，校对
  产物标 `boundary:"estimated"` + `estimated_boundary` 审核项——绝不
  伪造词级时间戳。
- **字段差异防御**：`sentence_info`（VAD 路径）→ 顶层 `text` →
  `value`/`sentence` 依次尝试；实际命中的字段名写进 `backend.
  timestamp_fields` 供审计。SenseVoice 富文本标签用
  `rich_transcription_postprocess`，import 失败退化正则清洗，原文保留在
  `text_raw`。
- **长音频**：优先靠 FunASR VAD 内部切块；仅当 `vad:false` 且时长超过
  `--chunk-sec`（默认 300s）时 Go 侧兜底分块——能量包络找窗口内最低点
  下刀，块 WAV 落在 `out/chunks/` 可审计，偏移恢复写进 `chunks[]`。

## B. 校对（reconcile）

确定性算法，无 LLM 依赖（语义相似度只允许作加分项，不能推翻顺序约束）：

0. **融合段预切**：`SplitFusedSegments` 先把无 VAD 的整段切成
   语句级段：切点是句末标点，其次词级时间戳上 ≥600ms 的停顿
   （ASR 可能整段不给标点）；词 ts 派生的边界不标估计，按比例
   推算的内部切点一律标 `coarse`（父段的真实外边界不外推）。
   既无标点又无词 ts 的「不可切段」不硬切——块匹配对其放宽行数
   上限（≤64 行），产物全部 `estimated_boundary`+待审核并在
   `timeline.warnings` 记降级说明，而不是误报大片 `no_audio`。
   外部 asr-raw 也走同一入口。
1. **归一化**：去标点空白、全角折半角、小写；保留原文字符索引映射，
   使切回原文取 `spoken_text` 时每个真实字符都能还原。
2. **块级 DP**：`lines × segs` 网格上的最小代价路径，块 ≤
   `MaxLinesPerBlock×MaxSegsPerBlock`（默认 4×6）。匹配代价
   `1−F1 + MergePenalty·(di−1+dk−1)`；跳行 0.9/行，跳段 0.7/段。
   F1 = `2·匹配字符数/(行长度+段长度)`——比 NW 分数归一化宽容，
   同义改写（等下啊×等一下）不被打成 0。
3. **块内字符级 NW**（+2/−1/−2）把文本归属切到行；`aligned`（M+S）
   算覆盖率，`matches`（仅 M）算相似度——替换字符覆盖行但不计分。
4. **剥离**：段覆盖率 < `PeelSimilarity`（0.35）→ 段转 unmatched；
   行覆盖率 < `LineCoverageFloor` 或行内 span 相似度 < `LineSimFloor`
   （0.15）→ 行转 `unmatched_lines`，其音频段随之回到 unmatched 池——
   防止漏读行蹭邻居音频、或前一句的重读段被错钉到下一句。
5. **位置归属**：未被对齐的 B 侧字符（口头禅、重复词头）按就近原则
   归并到相邻行的 span，保证 `spoken_text` 里每个真实朗读的字都不丢。
6. **take / part 分离**：unmatched 段若与邻近已绑定行 sim ≥
   `TakeSimilarity`（0.55）或为行首前缀（≥3 字）→ 该行的候选 take；
   `finalizeTakes` 按 solo 相似度选最优，差距 < `TakeAmbiguity`
   （0.08）时标 `ambiguous_takes` 交人工。同一行被停顿拆成的多个段
   不是 take——它们保持 `take_index=0` 并按时间拿 `part_index`
   (1..N)；「同一 line_id 出现多次」要区分时看这两个字段。所有
   take 都保留在 timeline，未选中的进 `manifest.skipped`，原始数据
   永不删。
7. **审核协议**：`review_required` + `review_reasons`（
   `low_similarity`/`estimated_boundary`/`ambiguous_takes`/
   `unmatched_audio`/`low_asr_confidence`/`needs_role`）。
   人工编辑写 `edited:true`（只标记被碰过）；显式确认写
   `reviewed:true` 才解除审核门——改文本/角色/勾选不等于审核。
   `MergeReviewed` 以（line_id, asr_ref, ±2s）为键把编辑和确认并回
   新草稿——重跑校对不丢人工工作。两份文件 `audio_sha256` 不符时
   直接拒绝合并（旧审核不许嫁接新录音）。未匹配音频人工指定角色
   后置为 `match_status:"manual"`（人工绑定=审核决定），与正常行
   一样参与渲染。**渲染默认拦截未审核段**进 `skipped`；
   `--allow-unreviewed` 显式放行并逐段记 `manifest.warnings`。
   `manifest.status`：`complete`/`blocked_review`/`partial`/`failed`——
   整句漏读（`unmatched_lines` 非空）报 `partial`，仅当人工在
   timeline 将该行标 `optional:true` 才豁免；`partial` 优先级高于
   `blocked_review`（缺失音频靠审核补不回来）。`manifest.counts`
   给出结构化原因计数（rendered/failures/blocked_review/
   missing_lines/unselected_takes），agent 无需解析 skipped 文案。

## C. 渲染（render）

- **切分不硬切词边界**：`[start,end]` 先在能量包络上 refine（start 在
  −250/+150ms、end 在 −150/+250ms 内找最低点），再向两侧取上下文
  （前 ≤300ms、后 ≤300ms）喂引擎，渲染完裁回——DSP 的冷启动 ramp 落在
  被裁掉的上下文上，段间不互相串状态。
- **每段独立 `engine.New()`**：`SetParams` 是流式切换语义（滤波器状态、
  WSOLA 搜索窗、PitchTracker 中位数窗都带历史）；跨段复用会把上段尾部
  掺进下段头部。独立换来并行（`--jobs`）和缓存。
- **缓存即内容寻址**：`lines/<segment_id>-<sha8>.wav`，key 覆盖
  seg_id + 切点 + 音频 sha + 规范化参数；key 变 = 新文件，旧产物永不
  被覆盖。
- **轨道**：`roles/<role>.wav` 与源等长，段按 `cut_start_ms` 写回原
  位置（停顿全部保留），相邻 refined 区间相接处 ≤20ms 线性 crossfade；
  `mix.wav` 同法放全部角色，峰值 >1 时整体统一缩增益并记 warning。
- **校验回读**：写完即解码回读，长度对不上就进 `manifest.warnings`——
  不静默。
- **失败隔离**：`--keep-going` 单段失败继续并记入 `failures[]`；非
  keep-going 模式下 manifest 不写，旧产物保持一致。
- **不渲染的**：漏读行、未选中 take、unmatched 段一律不进产物，全部
  进 `manifest.skipped[]` 带原因。

## Schema 一览

| 文件 | 谁写谁读 | 要点 |
|---|---|---|
| `script.json` | 外部 agent 写 | `lines[]:{line_id,scene_id,role_id,text,direction}` |
| `asr-raw.json` | analyze 写 | 原始 ASR 证据 + backend capability + chunks |
| `timeline(-reviewed).json` | reconcile 写/人改 | segments + unmatched_lines + review 标记 |
| `voices.json` | 人写 | role → `{backend:"dsp", preset}` 或完整 params |
| `rendered/manifest.json` | render 写/agent 读 | lines/skipped/failures/roles/mix/subtitles/warnings |

所有时间毫秒、采样率 48k、版本字段必检。`natural-woman`/`natural-child`
预设是实验性的，manifest 会如实标 `experimental:true`——它们不是
验证过的自然声线。

## 扩展点（明确没有实现的部分）

- `VoiceProfile.Backend` 目前只有 `"dsp"`；加神经 VC 后端时在
  `voices.go: resolveProfile` 和 `render.go` 的段渲染处分支，协议已预留。
- `ReconcileOptions.Scorer` 预留了外部语义打分钩子（只能加分不能改序）。
- UI 是 P1 最小审核器，不是 DAW；更精细的边界编辑走
  `timeline-reviewed.json` 手工改字段，render 直接吃它。
