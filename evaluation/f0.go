package evaluation

import "voice-morph/engine"

// DefaultPitchTracker uses the production F0 estimator so offline input,
// rendered output and optional reference clips are measured by the same
// algorithm that drives speech-aware rendering.
func DefaultPitchTracker(sampleRate int) PitchTracker {
	return pitchTrackerAdapter{pt: engine.NewPitchTracker(sampleRate)}
}

type pitchTrackerAdapter struct{ pt *engine.PitchTracker }

func (a pitchTrackerAdapter) Analyze(frame []float64) PitchEstimate {
	est := a.pt.Analyze(frame)
	return PitchEstimate{F0: est.F0, Confidence: est.Confidence, Voiced: est.Voiced}
}
