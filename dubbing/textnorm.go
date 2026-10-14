package dubbing

import "unicode"

// Text normalization for matching: keep letters and digits only (CJK,
// latin, digits), lowercase, fold fullwidth ASCII into halfwidth.
// Punctuation and whitespace are dropped — they are delivery, not
// wording. The raw text is never modified; normalization lives only in
// the matcher's working copies.

// normRunes returns the normalized runes of s plus, for each kept rune,
// its index in the rune slice of s (so aligned spans can be mapped back
// to raw text).
func normRunes(s string) (norm []rune, src []int) {
	rs := []rune(s)
	for i, r := range rs {
		if nr, ok := normRune(r); ok {
			norm = append(norm, nr)
			src = append(src, i)
		}
	}
	return norm, src
}

// normText returns the normalized comparison form of s.
func normText(s string) string {
	n, _ := normRunes(s)
	return string(n)
}

// normRune folds one rune into its comparison form; ok=false drops it.
func normRune(r rune) (rune, bool) {
	// fullwidth ASCII → halfwidth (！～ｒ → !~r)
	if r >= 0xFF01 && r <= 0xFF5E {
		r -= 0xFEE0
	}
	switch {
	case unicode.IsDigit(r):
		return r, true
	case unicode.IsLetter(r):
		return unicode.ToLower(r), true
	}
	return 0, false
}

// editDist is the Levenshtein distance between two rune slices.
func editDist(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			sub := prev[j-1]
			if a[i-1] != b[j-1] {
				sub++
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, sub)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// similarity is 1 - editDist/maxLen: 1 identical, →0 unrelated.
func similarity(a, b []rune) float64 {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	if n == 0 {
		return 1
	}
	return 1 - float64(editDist(a, b))/float64(n)
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
