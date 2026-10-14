// Generates examples/dubbing/take.wav — deterministic synthetic audio
// standing in for a real take (real ASR needs SenseVoiceSmall on the
// target machine; this demo exercises reconcile+render deterministically).
//
// Layout matches asr-raw.json:
//   0.5-2.5s   l01 narrator "大家好，我们开始吧"
//   3.0-4.0s   seg-0003 unmatched audio (ad-lib, never in script)
//   5.0-6.5s   l02 character-a "等一下！" (paraphrased vs script)
//   7.5-9.0s   l03 take 1 "马上就好" (false start, truncated)
//   9.5-11.5s  l03 take 2 "马上就好" (complete take → selected)
//   l04 narrator "我们下午再练" has no audio at all (skipped line).
package main

import (
	"math"
	"os"

	"voice-morph/engine"
)

func main() {
	const sr = engine.SampleRate
	n := 12 * sr
	x := make([]float32, n)
	regions := [][2]int{
		{500, 2500}, {3000, 4000}, {5000, 6500}, {7500, 9000}, {9500, 11500},
	}
	for k, r := range regions {
		f0 := 140 + float64(k)*25
		a, b := r[0]*sr/1000, r[1]*sr/1000
		for i := a; i < b; i++ {
			t := float64(i) / sr
			env := 1.0
			// soft attack/release inside each region
			pos := float64(i-a) / float64(b-a)
			if pos < 0.05 {
				env = pos / 0.05
			} else if pos > 0.9 {
				env = (1 - pos) / 0.1
			}
			x[i] = float32(env * (0.25*math.Sin(2*math.Pi*f0*t) +
				0.08*math.Sin(2*math.Pi*2.7*f0*t)))
		}
	}
	if err := os.WriteFile("take.wav", engine.EncodeWAV(x, sr), 0o644); err != nil {
		panic(err)
	}
}
