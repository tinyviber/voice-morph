package engine

import "math"

// fft performs an in-place iterative radix-2 FFT. n must be a power of two.
// The inverse transform is scaled by 1/n.
func fft(buf []complex128, inverse bool) {
	n := len(buf)
	if n <= 1 {
		return
	}
	// bit-reversal permutation
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			buf[i], buf[j] = buf[j], buf[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		step := -2 * math.Pi / float64(size)
		if inverse {
			step = -step
		}
		for i := 0; i < n; i += size {
			for j := 0; j < half; j++ {
				ang := step * float64(j)
				w := complex(math.Cos(ang), math.Sin(ang))
				u := buf[i+j]
				v := buf[i+j+half] * w
				buf[i+j] = u + v
				buf[i+j+half] = u - v
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
