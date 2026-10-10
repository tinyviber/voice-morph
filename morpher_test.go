package main

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"voice-morph/engine"
)

// The chunk protocol's tests exercise Morpher.ProcessChunk directly —
// the same code path the mygo binding runs — with reference engines as
// the oracle: the DSP chain is deterministic, so a live stream's output
// must equal a fresh engine fed exactly the accepted blocks, bit for bit.

const testBlockLen = 512

func testParams() engine.Params {
	p := engine.DefaultParams
	p.Pitch = 0.5 // a non-identity chain: stream state visibly shapes output
	p.Timbre = 0.2
	return p
}

func newMorpher() *Morpher {
	m := &Morpher{eng: engine.New(), lastSeq: -1}
	m.eng.SetParams(testParams())
	return m
}

func newRef() *engine.Engine {
	e := engine.New()
	e.SetParams(testParams())
	// mirror the state a stream runs in: SetParams alone leaves the
	// engine mid-crossfade, while every live stream starts from a
	// rebuild (ResetStream / stream adoption).
	e.Reset()
	return e
}

func f32ToB64(x []float32) string {
	buf := make([]byte, len(x)*4)
	for i, v := range x {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func b64ToF32(t *testing.T, s string) []float32 {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("ProcessChunk returned bad base64: %v", err)
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

// constBlock builds an identifiable payload — a constant-valued block —
// so a stale call that secretly pushed its content into WSOLA/formant
// memory would shift every later output away from the reference.
func constBlock(v float32) (b64 string, pcm []float32) {
	pcm = make([]float32, testBlockLen)
	for i := range pcm {
		pcm[i] = v
	}
	return f32ToB64(pcm), pcm
}

func mustChunk(t *testing.T, m *Morpher, stream, seq int64, pcm string) []float32 {
	t.Helper()
	out, err := m.ProcessChunk(stream, seq, pcm)
	if err != nil {
		t.Fatalf("ProcessChunk(stream=%d, seq=%d) rejected a live call: %v", stream, seq, err)
	}
	return b64ToF32(t, out)
}

func mustStale(t *testing.T, m *Morpher, stream, seq int64, pcm string) {
	t.Helper()
	out, err := m.ProcessChunk(stream, seq, pcm)
	if err == nil {
		t.Fatalf("ProcessChunk(stream=%d, seq=%d) accepted a stale call", stream, seq)
	}
	if out != "" {
		t.Fatalf("stale rejection returned output (%d bytes)", len(out))
	}
	if !strings.HasPrefix(err.Error(), staleChunkPrefix) {
		t.Fatalf("stale rejection must carry %q for the JS resync path, got %q",
			staleChunkPrefix, err.Error())
	}
}

// Same stream, out-of-order and duplicated seqs: every accepted call
// produces reference-equal output, every replay or laggard is refused.
func TestProcessChunkSeqOrdering(t *testing.T) {
	m := newMorpher()
	ref := newRef()
	s := m.ResetStream()

	a, aIn := constBlock(0.05)
	b, bIn := constBlock(0.10)
	c, cIn := constBlock(0.15)

	if got := mustChunk(t, m, s, 0, a); !slices.Equal(got, ref.Process(aIn)) {
		t.Fatal("seq 0 output diverges from the reference stream")
	}
	if got := mustChunk(t, m, s, 1, b); !slices.Equal(got, ref.Process(bIn)) {
		t.Fatal("seq 1 output diverges from the reference stream")
	}
	// replays and laggards of an already-processed seq are dead
	mustStale(t, m, s, 1, c) // duplicate seq with a different payload
	mustStale(t, m, s, 0, c) // replayed seq 0
	if m.staleDrops != 2 {
		t.Fatalf("staleDrops = %d, want 2", m.staleDrops)
	}
	// a forward jump is a content hole, not an error: counted, processed
	if got := mustChunk(t, m, s, 4, c); !slices.Equal(got, ref.Process(cIn)) {
		t.Fatal("seq 4 output diverges from the reference stream")
	}
	if m.seqGaps != 2 {
		t.Fatalf("seqGaps = %d, want 2 (seqs 2,3 dropped client-side)", m.seqGaps)
	}
	// and the skipped seq can never slip in afterwards
	mustStale(t, m, s, 2, a)
	mustStale(t, m, s, 3, b)
}

// A stale-seq call must not push its payload into the engine's stream
// memory: the next live output stays byte-identical to a reference that
// never saw the marker.
func TestProcessChunkStaleSeqZeroMutation(t *testing.T) {
	m := newMorpher()
	ref := newRef()
	s := m.ResetStream()

	mustChunk(t, m, s, 0, mustB64(t, ref, 0.05))
	marker, _ := constBlock(0.95)
	mustStale(t, m, s, 0, marker) // replay seq 0 carrying the marker

	next, nextIn := constBlock(0.07)
	if got := mustChunk(t, m, s, 1, next); !slices.Equal(got, ref.Process(nextIn)) {
		t.Fatal("stale call contaminated stream state: next output diverges")
	}
}

// mustB64 feeds the reference engine the same constant block and returns
// the block's base64 encoding (for feeding the morpher).
func mustB64(t *testing.T, ref *engine.Engine, v float32) string {
	t.Helper()
	s, pcm := constBlock(v)
	ref.Process(pcm)
	return s
}

func b64(v float32) string {
	s, _ := constBlock(v)
	return s
}

// The resync path: the sender bumps the stream ID without a ResetStream
// round trip. ProcessChunk adopts the higher ID, rebuilds stream state,
// and the old stream is dead — even at higher seqs.
func TestProcessChunkNewStreamAdoptsAndResets(t *testing.T) {
	m := newMorpher()
	s1 := m.ResetStream()
	mustChunk(t, m, s1, 0, b64(0.05)) // build state on stream 1
	mustChunk(t, m, s1, 1, b64(0.06))

	// resync: fresh stream ID, seq restarts at 0 — the engine output must
	// match a brand-new reference stream bit for bit (the adoption reset
	// wiped stream 1's WSOLA/formant memory).
	s2 := s1 + 1
	ref2 := newRef()
	x, xIn := constBlock(0.08)
	if got := mustChunk(t, m, s2, 0, x); !slices.Equal(got, ref2.Process(xIn)) {
		t.Fatal("adopted stream did not start from rebuilt state")
	}

	// a zombie call on the old stream is refused at ANY seq — even one
	// the old stream never reached.
	marker, _ := constBlock(0.95)
	mustStale(t, m, s1, 9, marker)

	// and stream 2 continues to track only its own content
	y, yIn := constBlock(0.09)
	if got := mustChunk(t, m, s2, 1, y); !slices.Equal(got, ref2.Process(yIn)) {
		t.Fatal("old-stream zombie contaminated the new stream")
	}
}

// The review scenario: a call on stream 1 is still running when the
// sender has already moved to stream 2. Whichever goroutine wins the
// mutex, stream 2 must end up as the only live stream — the zombie is
// either rejected outright (it ran second) or its mutation is wiped by
// the adoption reset (it ran first).
func TestProcessChunkTimedOutCallCannotPoisonResync(t *testing.T) {
	for range 20 { // run the race a few ways; the end state is deterministic
		m := newMorpher()
		s1 := m.ResetStream()
		s2 := s1 + 1
		mustChunk(t, m, s1, 0, b64(0.05))
		mustChunk(t, m, s1, 1, b64(0.06))

		marker, _ := constBlock(0.95)
		x, xIn := constBlock(0.08)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = m.ProcessChunk(s1, 2, marker) }() // zombie
		go func() { defer wg.Done(); _, _ = m.ProcessChunk(s2, 0, x) }()      // resync head
		wg.Wait()

		ref := newRef()
		ref.Process(xIn)
		y, yIn := constBlock(0.09)
		if got := mustChunk(t, m, s2, 1, y); !slices.Equal(got, ref.Process(yIn)) {
			t.Fatal("stream 2 output diverges — the zombie's mutation survived")
		}
	}
}

// A monitor restart (ResetStream) kills every stream issued before it,
// including calls still in flight when the restart landed.
func TestResetStreamKillsInFlightStreams(t *testing.T) {
	m := newMorpher()
	s1 := m.ResetStream()
	mustChunk(t, m, s1, 0, b64(0.05))

	// "in flight" during the restart: sent on s1, lands after it
	marker, _ := constBlock(0.95)
	s2 := m.ResetStream()
	mustStale(t, m, s1, 1, marker)

	ref := newRef()
	x, xIn := constBlock(0.08)
	if got := mustChunk(t, m, s2, 0, x); !slices.Equal(got, ref.Process(xIn)) {
		t.Fatal("post-restart output diverges — the dead stream leaked")
	}
}

// mygo runs every bound call on its own goroutine: under concurrency the
// check+process critical section must admit only the increasing
// subsequence of arrivals — a seq that reaches the engine after a higher
// seq is refused, never replayed, and no seq executes twice.
func TestProcessChunkConcurrentUniqueSeqs(t *testing.T) {
	m := newMorpher()
	s := m.ResetStream()
	const n = 16

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.ProcessChunk(s, int64(i), b64(float32(i+1)*0.01))
		}(i)
	}
	wg.Wait()

	var accepted []int64
	for i, err := range errs {
		switch {
		case err == nil:
			accepted = append(accepted, int64(i))
		case !strings.HasPrefix(err.Error(), staleChunkPrefix):
			t.Fatalf("seq %d got a non-protocol error: %v", i, err)
		}
	}
	if len(accepted) == 0 {
		t.Fatal("no call processed at all")
	}
	if !slices.IsSorted(accepted) {
		t.Fatalf("accepted seqs %v — a lower seq executed after a higher one", accepted)
	}
	if m.lastSeq != n-1 {
		t.Fatalf("lastSeq = %d, want %d (seq %d always lands last)", m.lastSeq, n-1, n-1)
	}

	// every one of those seqs is now dead, however it arrives
	var stale int64
	var mu sync.Mutex
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.ProcessChunk(s, int64(i), b64(0.95)); err != nil {
				mu.Lock()
				stale++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if stale != n {
		t.Fatalf("replays accepted: %d of %d rejected", stale, n)
	}
}

// A malformed payload fails the call without consuming its seq — the
// sender can lose the block (drop) and the stream continues.
func TestProcessChunkBadPayloadDoesNotConsumeSeq(t *testing.T) {
	m := newMorpher()
	ref := newRef()
	s := m.ResetStream()

	mustChunk(t, m, s, 0, mustB64(t, ref, 0.05))
	if _, err := m.ProcessChunk(s, 1, "!!!not-base64!!!"); err == nil {
		t.Fatal("bad payload was accepted")
	}
	// seq 1 was never processed → a later seq-1 call is still valid, and
	// its output matches the reference that never saw a seq-1 block
	b, bIn := constBlock(0.10)
	if got := mustChunk(t, m, s, 1, b); !slices.Equal(got, ref.Process(bIn)) {
		t.Fatal("seq 1 output diverges after the bad payload")
	}
}
