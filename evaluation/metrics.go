// Package evaluation contains the pure-Go offline metrics used by the
// voice-eval command. It deliberately depends only on the engine package
// and the standard library so reports remain reproducible on any machine
// that can build this repository.
package evaluation

import (
	"math"
	"sort"
)

// SignalStats describes the level and voiced-pitch profile of one clip.
// Frequencies are reported in hertz and as musical pitch (MIDI semitones,
// where 69 is A4); a range of zero means no reliable voiced frame.
type SignalStats struct {
	DurationSec   float64 `json:"durationSec"`
	SampleRate    int     `json:"sampleRate"`
	Samples       int     `json:"samples"`
	RMS           float64 `json:"rms"`
	RMSDBFS       float64 `json:"rmsDbfs"`
	Peak          float64 `json:"peak"`
	PeakDBFS      float64 `json:"peakDbfs"`
	VoicedRatio   float64 `json:"voicedRatio"`
	VoicedFrames  int     `json:"voicedFrames"`
	TotalFrames   int     `json:"totalFrames"`
	VoicedSeconds float64 `json:"voicedSeconds"`
	F0MedianHz    float64 `json:"f0MedianHz"`
	F0P10Hz       float64 `json:"f0P10Hz"`
	F0P90Hz       float64 `json:"f0P90Hz"`
	F0MinHz       float64 `json:"f0MinHz"`
	F0MaxHz       float64 `json:"f0MaxHz"`
	SemitoneRange float64 `json:"semitoneRange"`
	MedianMidi    float64 `json:"medianMidi"`
	P10Midi       float64 `json:"p10Midi"`
	P90Midi       float64 `json:"p90Midi"`
}

// Analysis framing follows the M0 brief: 3072-sample frames on a 512 hop
// at 48 kHz.
const (
	defaultFrameSize = 3072
	defaultHop       = 512
)

// PitchEstimate is the minimal contract the metrics layer needs from an
// F0 tracker; DefaultPitchTracker adapts engine.PitchEstimate to it.
type PitchEstimate struct {
	F0         float64
	Confidence float64
	Voiced     bool
}

// PitchTracker is the narrow F0 interface AnalyzePitch depends on, so a
// test or reference tracker can replace the production one.
type PitchTracker interface {
	Analyze(frame []float64) PitchEstimate
}

// AnalyzePitch runs tracker over the FrameCount frames covering the clip
// and returns the accepted voiced estimates. The last frame is zero-padded,
// so a clip shorter than one frame is still analyzed once.
func AnalyzePitch(samples []float32, sr int, tracker PitchTracker, frameSize, hop int, minConf float64) []PitchEstimate {
	if sr <= 0 || tracker == nil {
		return nil
	}
	frameSize, hop = framing(frameSize, hop)
	frame := make([]float64, frameSize)
	var out []PitchEstimate
	for k := range FrameCount(len(samples), frameSize, hop) {
		start := k * hop
		clear(frame)
		for i := 0; i < frameSize && start+i < len(samples); i++ {
			frame[i] = float64(samples[start+i])
		}
		est := tracker.Analyze(frame)
		if est.Voiced && est.Confidence >= minConf && est.F0 > 0 && !math.IsNaN(est.F0) && !math.IsInf(est.F0, 0) {
			out = append(out, est)
		}
	}
	return out
}

// Summarize computes level statistics and derives F0 summaries from a
// pitch-estimate series produced by AnalyzePitch. totalFrames is passed
// separately so an all-unvoiced clip still reports the correct voiced
// ratio denominator.
func Summarize(samples []float32, sr int, estimates []PitchEstimate, totalFrames int) SignalStats {
	s := SignalStats{SampleRate: sr, Samples: len(samples)}
	if sr > 0 {
		s.DurationSec = float64(len(samples)) / float64(sr)
	}
	var sum float64
	for _, v := range samples {
		a := math.Abs(float64(v))
		if a > s.Peak {
			s.Peak = a
		}
		sum += float64(v) * float64(v)
	}
	if len(samples) > 0 {
		s.RMS = math.Sqrt(sum / float64(len(samples)))
	}
	s.RMSDBFS = dbfs(s.RMS)
	s.PeakDBFS = dbfs(s.Peak)
	s.TotalFrames = totalFrames
	s.VoicedFrames = len(estimates)
	if totalFrames > 0 {
		s.VoicedRatio = float64(len(estimates)) / float64(totalFrames)
		if len(samples) > 0 {
			s.VoicedSeconds = s.DurationSec * s.VoicedRatio
		}
	}
	if len(estimates) == 0 {
		return s
	}
	f0 := make([]float64, len(estimates))
	for i, e := range estimates {
		f0[i] = e.F0
	}
	sort.Float64s(f0)
	s.F0MinHz, s.F0MaxHz = f0[0], f0[len(f0)-1]
	s.F0MedianHz = quantile(f0, 0.5)
	s.F0P10Hz = quantile(f0, 0.1)
	s.F0P90Hz = quantile(f0, 0.9)
	s.MedianMidi = hzToMidi(s.F0MedianHz)
	s.P10Midi = hzToMidi(s.F0P10Hz)
	s.P90Midi = hzToMidi(s.F0P90Hz)
	if s.F0MinHz > 0 {
		s.SemitoneRange = 12 * math.Log2(s.F0MaxHz/s.F0MinHz)
	}
	return s
}

// FrameCount is the number of hop-spaced frames AnalyzePitch analyzes: the
// fewest whose last frame reaches the end of the clip. It is the voiced
// ratio's denominator.
func FrameCount(samples, frameSize, hop int) int {
	frameSize, hop = framing(frameSize, hop)
	if samples <= 0 {
		return 0
	}
	if samples <= frameSize {
		return 1
	}
	return 1 + (samples-frameSize+hop-1)/hop
}

// framing substitutes the defaults for non-positive frame and hop sizes.
func framing(frameSize, hop int) (int, int) {
	if frameSize <= 0 {
		frameSize = defaultFrameSize
	}
	if hop <= 0 {
		hop = defaultHop
	}
	return frameSize, hop
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	q = math.Max(0, math.Min(1, q))
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	w := pos - float64(lo)
	return sorted[lo]*(1-w) + sorted[hi]*w
}

func hzToMidi(hz float64) float64 {
	if hz <= 0 {
		return 0
	}
	return 69 + 12*math.Log2(hz/440)
}

func dbfs(x float64) float64 {
	if x <= 0 {
		return -math.MaxFloat64
	}
	return 20 * math.Log10(x)
}
