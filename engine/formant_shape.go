package engine

import "math"

// piecewiseWarp maps a log-frequency position to another using control
// points. Unlike the legacy global scale it can raise F1 differently from
// F2, matching the vowel-dependent vocal-tract statistics in the report.
// ctrl is a monotone log-src/log-dst table; the ends continue linearly.
func piecewiseWarp(logSrc []float64, ctrl [][2]float64, shape float64) []float64 {
	out := make([]float64, len(logSrc))
	for i, f := range logSrc {
		dst := f
		switch {
		case f <= ctrl[0][0]:
			slope := (ctrl[1][1] - ctrl[0][1]) / (ctrl[1][0] - ctrl[0][0])
			dst = ctrl[0][1] + slope*(f-ctrl[0][0])
		case f >= ctrl[len(ctrl)-1][0]:
			l := len(ctrl) - 1
			slope := (ctrl[l][1] - ctrl[l-1][1]) / (ctrl[l][0] - ctrl[l-1][0])
			dst = ctrl[l][1] + slope*(f-ctrl[l][0])
		default:
			for j := 1; j < len(ctrl); j++ {
				if f <= ctrl[j][0] {
					a, b := ctrl[j-1], ctrl[j]
					u := (f - a[0]) / (b[0] - a[0])
					dst = a[1] + u*(b[1]-a[1])
					break
				}
			}
		}
		// Shape ±1 reaches the giant/short-tract table. The sign of the
		// correction flips when shape does; |shape| is its gain.
		out[i] = f + shape*(dst-f)
	}
	return out
}

// speechWarpTable is the M2 vocabulary-independent approximation of
// vowel-dependent tract scaling, in log2(Hz). Positive shape raises F1
// more than F2 (Hillenbrand's male→female/child ratio asymmetry); negative
// shape mirrors it for giant voices. Timbre still sets the global VTL
// amount on top of this shape.
func speechWarpTable() [][2]float64 {
	l2 := math.Log(2)
	f := func(hz float64) float64 { return math.Log(hz) / l2 }
	return [][2]float64{
		// src, dst at neutral timbre: only the shape term applies.
		{f(80), f(80)},
		{f(250), f(238)},
		{f(500), f(470)},
		{f(900), f(815)}, // F1 region shifts more strongly
		{f(1600), f(1510)},
		{f(2600), f(2510)}, // F2 shifts less
		{f(4500), f(4420)},
		{f(8000), f(8000)},
		{f(16000), f(16000)},
		{f(22050), f(22050)},
	}
}
