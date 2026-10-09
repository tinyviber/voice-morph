package engine

import (
	"math"
	"sync"
)

// fftPlans caches per-transform-size tables: the bit-reversal
// permutation and the twiddle factors (roots of unity) for every
// radix-2 stage. FFT sizes repeat across frames, so building these
// once removes the math.Sin/Cos calls that used to run inside every
// butterfly. Entries hold exactly the values the inline loop computed:
// tw[j] = complex(cos(step*j), sin(step*j)) with the same step and
// angle expressions, so results stay bit-identical. Sizes in use are
// few, so unbounded growth is not a concern.
var fftPlans sync.Map // fftPlanKey -> *fftPlan

type fftPlanKey struct {
	n       int
	inverse bool
}

type fftPlan struct {
	rev []int // bit-reversal permutation, rev[0] = 0
	// tw[s][j] is the twiddle factor for butterfly j of stage s, whose
	// transform size is 2<<s (i.e. 2,4,8,...,n)
	tw [][]complex128
}

func getFFTPlan(n int, inverse bool) *fftPlan {
	k := fftPlanKey{n, inverse}
	if v, ok := fftPlans.Load(k); ok {
		return v.(*fftPlan)
	}
	p := &fftPlan{rev: make([]int, n)}
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		p.rev[i] = j
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		t := make([]complex128, half)
		step := -2 * math.Pi / float64(size)
		if inverse {
			step = -step
		}
		for j := 0; j < half; j++ {
			ang := step * float64(j)
			t[j] = complex(math.Cos(ang), math.Sin(ang))
		}
		p.tw = append(p.tw, t)
	}
	v, _ := fftPlans.LoadOrStore(k, p)
	return v.(*fftPlan)
}

// fft performs an in-place iterative radix-2 FFT. n must be a power of two.
// The inverse transform is scaled by 1/n.
func fft(buf []complex128, inverse bool) {
	n := len(buf)
	if n <= 1 {
		return
	}
	p := getFFTPlan(n, inverse)
	// bit-reversal permutation
	rev := p.rev
	for i := 1; i < n; i++ {
		if j := rev[i]; i < j {
			buf[i], buf[j] = buf[j], buf[i]
		}
	}
	stage := 0
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		tw := p.tw[stage][:half]
		stage++
		for i := 0; i < n; i += size {
			blk := buf[i : i+size]
			for j := 0; j < half; j++ {
				w := tw[j]
				u := blk[j]
				v := blk[j+half] * w
				blk[j] = u + v
				blk[j+half] = u - v
			}
		}
	}
	if inverse {
		inv := complex(1/float64(n), 0)
		for i := range buf {
			buf[i] *= inv
		}
	}
}

// rfft computes |FFT(x)| for real input x; out must have n/2+1 entries.
func rfft(x, win []float64, buf []complex128, out []complex128) {
	for i := range buf {
		buf[i] = complex(x[i]*win[i], 0)
	}
	fft(buf, false)
	copy(out, buf[:len(out)])
}
