package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// DecodeWAV reads PCM and IEEE-float WAV data into mono float32 samples
// in [-1, 1] plus the file's sample rate.
func DecodeWAV(data []byte) ([]float32, int, error) {
	r := bytes.NewReader(data)
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, 0, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a WAVE file")
	}
	var (
		format, bits, channels int
		sr                     int
		pcm                    []byte
	)
	for r.Len() >= 8 {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, 0, err
		}
		size := int(binary.LittleEndian.Uint32(hdr[4:8]))
		want := size + size%2
		if want < 0 || want > r.Len() {
			return nil, 0, io.ErrUnexpectedEOF // declared size exceeds the file
		}
		body := make([]byte, want)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, 0, err
		}
		switch string(hdr[0:4]) {
		case "fmt ":
			if len(body) < 16 {
				return nil, 0, errors.New("WAVE: short fmt chunk")
			}
			format = int(binary.LittleEndian.Uint16(body[0:2]))
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			sr = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
		case "data":
			pcm = body[:size]
		}
	}
	if pcm == nil || channels < 1 || sr < 1 || bits < 8 {
		return nil, 0, errors.New("WAVE: missing or invalid fmt/data chunk")
	}
	// only these (format, bits) pairs are supported; validate before
	// dividing by bits/8 so malformed headers error instead of panic
	supported :=
		(format == 1 && (bits == 8 || bits == 16 || bits == 24 || bits == 32)) ||
			(format == 3 && (bits == 32 || bits == 64))
	if !supported {
		return nil, 0, fmt.Errorf("WAVE: unsupported format %d/%d-bit", format, bits)
	}
	frames := len(pcm) / channels / (bits / 8)
	out := make([]float32, frames)
	for i := 0; i < frames; i++ {
		var acc float64
		for ch := 0; ch < channels; ch++ {
			o := (i*channels + ch) * (bits / 8)
			switch {
			case format == 1 && bits == 16:
				acc += float64(int16(binary.LittleEndian.Uint16(pcm[o:]))) / 32768
			case format == 1 && bits == 8:
				acc += (float64(pcm[o]) - 128) / 128
			case format == 1 && bits == 24:
				v := int32(pcm[o]) | int32(pcm[o+1])<<8 | int32(pcm[o+2])<<16
				if v&0x800000 != 0 {
					v |= ^0xffffff
				}
				acc += float64(v) / 8388608
			case format == 1 && bits == 32:
				acc += float64(int32(binary.LittleEndian.Uint32(pcm[o:]))) / 2147483648
			case format == 3 && bits == 32:
				acc += float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[o:])))
			case format == 3 && bits == 64:
				acc += math.Float64frombits(binary.LittleEndian.Uint64(pcm[o:]))
			default:
				return nil, 0, fmt.Errorf("WAVE: unsupported format %d/%d-bit", format, bits)
			}
		}
		out[i] = float32(acc / float64(channels))
	}
	return out, sr, nil
}

// EncodeWAV writes mono float32 samples as a 16-bit PCM WAV.
func EncodeWAV(samples []float32, sr int) []byte {
	var b bytes.Buffer
	dataSize := len(samples) * 2
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataSize))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&b, binary.LittleEndian, uint32(sr))
	binary.Write(&b, binary.LittleEndian, uint32(sr*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataSize))
	for _, s := range samples {
		v := math.Max(-1, math.Min(1, float64(s)))
		binary.Write(&b, binary.LittleEndian, int16(v*32767))
	}
	return b.Bytes()
}
