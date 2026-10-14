# VoiceMorph 变声工坊

MorphVOX Pro 风格的实时变声器 —— Go DSP 引擎 + Web 前端 + MyGo 桌面壳。

![tech](https://img.shields.io/badge/stack-Go%201.27%20%C2%B7%20MyGo%20%C2%B7%20Bun%2FVite%20%C2%B7%20WebAudio-d97757)

## MorphVOX Pro 的调参模型（分析）

MorphVOX Pro 的核心是 Tweak Panel 上的三个参数 + 两段 EQ：

| 参数 | 官方语义 | 范围 | 本质 |
|------|----------|------|------|
| **Pitch Shift** | 基频（f0）平移，决定"说话调门高低" | −1.0 … +1.0（≈±1 个八度） | 基频变换 |
| **Timbre Shift** | "harmonic quality"（谐波整体质感），声带长短/共振峰位置 | ≈ −0.3（巨人）… +0.4（小孩） | 共振峰偏移，独立于基频 |
| **Timbre Strength** | 上述音色变换的作用强度 | 0–100% | 干湿比 |
| **10-band EQ** | 全局双十段均衡，可选作用于原始信号（pre）或输出信号（post） | ±12 dB | RBJ 峰值双二阶级联 |

官方预设的数值印证了这个拆分：`低沉 (-0.5, 0, 100%)` 只降调门；`巨人 (-0.5, -0.3, 100%)` 降调门**且**下移共振峰；`小孩 (+0.8, +0.4, 100%)` 两者同时抬升。**Pitch 改变"调"，Timbre 改变"嗓子的体格"** —— 两者正交，这就是 MorphVOX 声音塑造的全部秘密。

## 语音模式（speech path）

MorphVOX 的重采样 + WSOLA 路径把整段音频一起变速：辅音、呼吸和停顿会被同样升降调，听感偏"乐器"。新增的**语音模式**只对有声段做音高变换：

```
输入 ──> PitchTracker(YIN) ──> 浊音帧 → 1:1 TD-PSOLA ──> 谱包络
                                (清音/气声原样透传)      非均匀声道扭曲 ──> 输出
```

- **PitchTracker**（`engine/pitch.go`）：无分配的 YIN 式估计器，40–64 ms 帧、浊音段用滞回阈值（voiced run 内放宽门限）抗八度跳变。
- **TD-PSOLA**（`engine/speech_pitch.go`）：固定 hop 网格上 `logF0_out = center + pitch·ln2 + pitchRange·(logF0 − mu)`；`mu` 是说话人音高的滑动中位数，`pitchRange` 围绕它缩放语调起伏。清音帧直接旁通干声，不再走音高变换。
- **谱包络非均匀扭曲**（`engine/formant_shape.go`）：在 `Timbre` 的全局缩放之上叠加 `FormantShape`（−1 厚实 → +1 明亮/短声道），按 log 频率的声道长度统计量把 F1 提得比 F2 多，逼近真实的男女/成人–童声道差异。

参数：`speech` 开关、`pitchRange`（0–1.5，1 保持语调）、`targetF0`（70–500 Hz 目标音高中心，0 = 跟随输入）、`formantShape`（−1..1）。两个新增预设 `natural-woman` / `natural-child` 走语音路径；旧的 `woman`/`child` 仍走原路径。前端在调参面板顶部有"语音模式"开关，开启后出现三个对应滑杆。

## 离线评测 `voice-eval`

`cmd/voice-eval` 是纯 Go 的离线评测器：把一段 WAV 跑过任意预设或自定义参数，渲染 WAV、产出 JSON 报告（RMS/峰值/浊音率/各分位 F0）、可选生成盲听页或 CSV，并可与外部参考（如 Praat 目标音）对比。不依赖 Python 或网络。评测指标直接复用生产 `engine.PitchTracker`，所以报告与监听路径看到的是同一套音高估计。

```bash
go build ./cmd/voice-eval
./voice-eval -in testdata/cmu-arctic-slt-a0003.wav -preset all -report out/report.json -blind out/index.html
./voice-eval -list-presets            # 列出预设 ID
```

`testdata/` 附带两条 CMU Arctic 真实语音（`aew` 男声、`slt` 女声，CC 许可，含 `cmu_arctic_COPYING.txt`）；评测脚本与指标定义见 `docs/evaluation.md`。`/tmp` 下还做过一次 DSP vs Seed-VC 神经变声的盲听对比，结论记录在 `docs/neural-vc-evaluation.md`（音频与权重不入库）。

## 实现方式

```
mic/WebAudio ──┐
               ├─ pre-EQ ── 重采样(1/r) ── WSOLA(α=1/r) ── 共振峰偏移 ── post-EQ ── 增益/软限幅 ──> 输出
audio file ────┤        └────── Pitch = resample+WSOLA 复合 ──────┘   └── Timbre = 倒谱谱包络扭曲 ──┘
               └─ 语音模式 ── PitchTracker ── TD-PSOLA(浊音) ── 非均匀谱包络扭曲 ──> 同上后级
```

- **Pitch**：正向重采样把"声带+声道"一起变速（基频和共振峰同倍率缩放），WSOLA 时域分析-合成恢复时长，净效果即音高平移。
- **Timbre**：对每帧做同态解卷积 —— Hann 加窗 → FFT → `log|X|` → 倒谱低通（lifter ≈ 2ms）→ 谱包络 → 按 `2^timbre` 做频率轴扭曲 → 把包络差异乘回原谱。Strength 控制扭曲量插值。
- **EQ**：10 段 RBJ peaking biquad，带中心和 MorphVOX 相同的 31.25/62.5/…/16k。
- 全部纯 Go 实现（自写 radix-2 FFT、窗函数 sinc 重采样、NCC 匹配的 WSOLA、倒谱分析），零 cgo，48 kHz 单声道，引擎延迟 ≈ 107 ms。

## 目录

```
main.go            Morpher 服务（IPC 绑定）+ 窗口入口
engine/            DSP 引擎：fft / resample / wsola / formant(+shape) / eq /
                   wav / presets / pitch(语音模式跟踪器) / speech_pitch(PSOLA)
                   / speech_chain
cmd/voice-eval/    离线评测 CLI：渲染 WAV + JSON 报告 + 盲听页
evaluation/        评测指标与报告生成（与引擎同一 PitchTracker）
testdata/          两条 CMU Arctic 真实语音（含 COPYing 与 README）
docs/              evaluation.md 与神经变声对比实验记录
src/               前端（Vite + TS）：main.ts UI 逻辑、bridge.ts IPC/mock、style.css
public/worklet.js  AudioWorklet：采集 2048 采样块 ↔ 播放回写
index.html         Claude 风格布局（暖纸底 + 衬线 + 珊瑚橙）
mygo.config.ts     macOS 打包配置（universal = intel + Apple Silicon）
```

## 构建 macOS 版（Intel + M 芯片通用包）

前置：Go ≥ 1.25、bun（或 npm）。在 Mac 上：

```bash
bun install
go run github.com/egoist/mygo/cmd/mygo@latest generate   # 生成 src/mygo.ts 绑定
bun run build:web                                        # 前端 → dist/
go run github.com/egoist/mygo/cmd/mygo@latest build -platform darwin/universal
# 产物：build/VoiceMorph.app + .dmg（arm64+amd64 lipo 合一，需 macOS 上运行 lipo/签名）
```

非 Mac 机器也可 `GOOS=darwin go build` 交叉验证二进制（本项目已是纯 Go 无依赖）。

## 开发

```bash
bun run dev:web                 # vite dev server，浏览器打开即看 UI（mock 引擎）
bun run typecheck               # tsc
CGO_ENABLED=0 go test ./...     # 引擎 + 评测单测（音高/PSOLA/共振峰/EQ/WAV/指标）
CGO_ENABLED=0 ./node_modules/.bin/mygo vet .
bun test                        # 前端流式队列 / monitor 生命周期测试
```

浏览器预览模式下桥接层自动降级为 mock（音频原样返回），方便只调样式。

## 预留但暂未实现

- Windows / Linux 打包（mygo `-platform windows|linux`；代码本身无平台依赖，WebAudio 路径通用）
- 系统级虚拟麦克风（把变声输出喂给其它应用）—— 需要 VB-Cable/BlackHole 类内核驱动，超出纯 Go webview 范围
- 降噪门、Voice Doctor 向导、回声/键盘音效
