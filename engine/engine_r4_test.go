package engine

import (
	"math"
	"testing"
)

// R4 regression — the reviewer's P0: switching the morph chain while
// emit runs ahead on the old side must still complete the crossfade.
//
// Deadlock mechanics: emit stays glued to prev's coverage frontier
// (inPos). A fresh chain can only emit content for position p after
// the input has reached ~p+warmup, so its coverage end lands behind
// emit by ~latency; every sample it produces is then stale and gets
// dropped, fade never advances and prev never retires — the new
// params never take over. This hits dry→wet and wet→wet in both
// directions. The fix caps emit: a short bounded stall lets cur's
// coverage catch up instead of letting emit run away forever.
func TestCrossfadeTakeover(t *testing.T) {
	const chunk = 2048
	switchAt := 23 * chunk // ~0.98 s into the stream
	for _, tc := range []struct {
		name     string
		from, to float64
		wantF0   float64 // expected tail f0 for a 200 Hz comb source
	}{
		{"dry-to-wet", 0, 0.5, 200 * math.Pow(2, 0.5)},
		{"wet-up", 0.5, 1, 400},
		{"wet-down", 1, -0.5, 200 * math.Pow(2, -0.5)},
	} {
		in := vowel(200, 900, 3.0)
		e := New()
		p := DefaultParams
		p.Pitch = tc.from
		e.SetParams(p)
		var out []float32
		for i := 0; i < len(in); i += chunk {
			if i == switchAt {
				p.Pitch = tc.to
				e.SetParams(p)
			}
			end := min(i+chunk, len(in))
			out = append(out, e.Process(in[i:end])...)
		}
		if e.prev != nil {
			t.Errorf("%s: transition never finished — prev still active, fade %d/%d",
				tc.name, e.fade, morphFadeLen)
		}
		if e.fade < morphFadeLen {
			t.Errorf("%s: fade stuck at %d/%d", tc.name, e.fade, morphFadeLen)
		}
		if len(out) < SampleRate/2 {
			t.Fatalf("%s: streamed only %d samples", tc.name, len(out))
		}
		got := f0Seg(out[len(out)-SampleRate/2:], SampleRate/500, SampleRate/80)
		if math.Abs(got-tc.wantF0)/tc.wantF0 > 0.05 {
			t.Errorf("%s: tail f0 = %.1f Hz, want ~%.1f — new params never took over",
				tc.name, got, tc.wantF0)
		}
	}
}

// R4 task B — counter-proof for the bypass blend-alignment dispute.
// A single wideband pulse landing right at the dry→wet toggle must
// appear exactly once in the output and within ±2 ms (96 samples) of
// its input position: the wet path emits content at the position that
// covers it, so pairing wet[k] with dry[k] at the toggle is already
// position-aligned. If this fails the blend really is offset and a
// dry delay line is warranted instead.
func TestBypassBoundaryPulse(t *testing.T) {
	const chunk = 2048
	toggleAt := 12 * chunk
	for _, off := range []int{-64, 0, 64} {
		in := make([]float32, SampleRate)
		pulseAt := toggleAt + off
		for i := 0; i < 10; i++ { // ~0.2 ms wideband tick
			in[pulseAt+i] = 0.8
		}
		e := New()
		p := DefaultParams
		p.Pitch = 0.5
		p.Bypass = true
		e.SetParams(p)
		var out []float32
		var swIn, swOut int
		for i := 0; i < len(in); i += chunk {
			if i == toggleAt {
				swIn, swOut = e.inPos, e.outPos
				p.Bypass = false
				e.SetParams(p)
			}
			end := min(i+chunk, len(in))
			out = append(out, e.Process(in[i:end])...)
		}
		out = append(out, e.Flush()...)
		t.Logf("off %+d: at toggle inPos=%d outPos=%d (D=%d)", off, swIn, swOut, swIn-swOut)
		onsets := burstOnsets(out, 0.15, 400)
		if len(onsets) != 1 {
			t.Fatalf("off %+d: %d pulse onsets in output at %v, want exactly one near %d",
				off, len(onsets), onsets, pulseAt)
		}
		if d := onsets[0] - pulseAt; d < -96 || d > 96 {
			t.Errorf("off %+d: pulse onset at %d, input at %d (Δ%+d, want |Δ| ≤ 96)",
				off, onsets[0], pulseAt, d)
		}
	}
}

// R4 task C — switch-storm merge: rapid consecutive SetParams must not
// stack transitions. The covering side keeps speaking, the in-between
// chain is dropped, cur rebuilds straight onto the newest target and
// the fade still completes — with the newest params audible.
func TestSwitchStormMerge(t *testing.T) {
	const chunk = 2048
	in := vowel(200, 900, 2.0)
	e := New()
	p := DefaultParams
	p.Pitch = -0.5
	e.SetParams(p)
	storm := []float64{0.3, 0.8, 0.5} // three switches in three blocks
	var out []float32
	for i := 0; i < len(in); i += chunk {
		b := i / chunk
		if b >= 10 && b < 10+len(storm) {
			p.Pitch = storm[b-10]
			e.SetParams(p)
		}
		end := min(i+chunk, len(in))
		out = append(out, e.Process(in[i:end])...)
	}
	if e.prev != nil {
		t.Errorf("storm: transition never finished — prev still active, fade %d/%d",
			e.fade, morphFadeLen)
	}
	if e.fade < morphFadeLen {
		t.Errorf("storm: fade stuck at %d/%d", e.fade, morphFadeLen)
	}
	want := 200 * math.Pow(2, 0.5)
	got := f0Seg(out[len(out)-SampleRate/2:], SampleRate/500, SampleRate/80)
	if math.Abs(got-want)/want > 0.05 {
		t.Errorf("storm: tail f0 = %.1f Hz, want ~%.1f — newest params never took over",
			got, want)
	}
}
