package pipeline

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"sync"
)

const (
	probeHead = 128 << 10 // covers an ID3 tag with small cover art
	probeTail = 64 << 10  // an MP4 moov or the last Ogg page
	// fallbackKbps prices audio whose length cannot be read from its
	// header: bytes at 64 kbit/s, about what speech in MP3, Opus or AAC
	// takes.
	fallbackKbps = 64
	// floorKbps bounds what a header may claim: the client writes it, so a
	// forged byte rate or sample count could make any file nearly free.
	// No file is priced shorter than its bytes at 1536 kbit/s, the rate of
	// 48 kHz 16-bit stereo PCM, above any compressed or common PCM format.
	floorKbps = 1536
)

// audioProbe watches the first file of an upload go by and keeps what
// estimating its duration needs: the start, the end and the size.
type audioProbe struct {
	mu      sync.Mutex
	claimed bool
	head    []byte
	tail    []byte
	size    int64
}

// tee returns r with its bytes observed, for the first file only.
func (p *audioProbe) tee(r io.Reader) io.Reader {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.claimed {
		return r
	}
	p.claimed = true
	return io.TeeReader(r, p)
}

func (p *audioProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.size += int64(len(b))
	if room := probeHead - len(p.head); room > 0 {
		p.head = append(p.head, b[:min(room, len(b))]...)
	}
	p.tail = append(p.tail, b...)
	if len(p.tail) > 2*probeTail {
		p.tail = append(p.tail[:0], p.tail[len(p.tail)-probeTail:]...)
	}
	return len(b), nil
}

// ms estimates the duration of the observed file; 0 when nothing was seen.
func (p *audioProbe) ms() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size == 0 {
		return 0
	}
	tail := p.tail
	if len(tail) > probeTail {
		tail = tail[len(tail)-probeTail:]
	}
	return audioMs(p.head, tail, p.size)
}

// audioMs reads the duration from a WAV, MP3, FLAC, Ogg, MP4/M4A or WebM
// header, but never less than the bytes at floorKbps, or else prices the
// bytes at fallbackKbps.
func audioMs(head, tail []byte, size int64) int64 {
	for _, f := range []func() int64{
		func() int64 { return wavMs(head, size) },
		func() int64 { return flacMs(head) },
		func() int64 { return oggMs(head, tail) },
		func() int64 { return mp4Ms(head, tail) },
		func() int64 { return webmMs(head) },
		func() int64 { return mp3Ms(head, size) },
	} {
		if ms := f(); ms > 0 {
			return max(ms, size*8/floorKbps)
		}
	}
	return size * 8 / fallbackKbps
}

func le16(b []byte) int64 { return int64(binary.LittleEndian.Uint16(b)) }
func le32(b []byte) int64 { return int64(binary.LittleEndian.Uint32(b)) }
func be32(b []byte) int64 { return int64(binary.BigEndian.Uint32(b)) }

func wavMs(h []byte, size int64) int64 {
	if len(h) < 12 || string(h[:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return 0
	}
	var byteRate int64
	for off := 12; off+8 <= len(h); {
		id, n, body := string(h[off:off+4]), le32(h[off+4:off+8]), off+8
		switch id {
		case "fmt ":
			if body+12 <= len(h) {
				byteRate = le32(h[body+8 : body+12])
			}
		case "data":
			if byteRate <= 0 {
				return 0
			}
			// Streaming writers leave the size 0 or 0xFFFFFFFF.
			if avail := size - int64(body); n == 0 || n > avail {
				n = avail
			}
			return n * 1000 / byteRate
		}
		if n > int64(len(h)) {
			return 0
		}
		off = body + int(n) + int(n&1)
	}
	return 0
}

func flacMs(h []byte) int64 {
	if len(h) < 26 || string(h[:4]) != "fLaC" || h[4]&0x7f != 0 {
		return 0
	}
	si := h[8:]
	rate := int64(si[10])<<12 | int64(si[11])<<4 | int64(si[12])>>4
	total := int64(si[13]&0x0f)<<32 | be32(si[14:18])
	if rate == 0 || total == 0 {
		return 0
	}
	return total * 1000 / rate
}

func oggMs(head, tail []byte) int64 {
	if len(head) < 28 || string(head[:4]) != "OggS" {
		return 0
	}
	pkt := 27 + int(head[26])
	if pkt+19 > len(head) {
		return 0
	}
	var rate, skip int64
	switch p := head[pkt:]; {
	case bytes.HasPrefix(p, []byte("\x01vorbis")):
		rate = le32(p[12:16])
	case bytes.HasPrefix(p, []byte("OpusHead")):
		rate, skip = 48000, le16(p[10:12])
	default:
		return 0
	}
	for i := bytes.LastIndex(tail, []byte("OggS")); i >= 0; i = bytes.LastIndex(tail[:i], []byte("OggS")) {
		if i+14 > len(tail) || tail[i+4] != 0 {
			continue
		}
		g := binary.LittleEndian.Uint64(tail[i+6 : i+14])
		if g == math.MaxUint64 || int64(g) <= skip || rate <= 0 {
			continue
		}
		return (int64(g) - skip) * 1000 / rate
	}
	return 0
}

func mp4Ms(head, tail []byte) int64 {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return 0
	}
	for _, b := range [][]byte{head, tail} {
		i := bytes.Index(b, []byte("mvhd"))
		if i < 0 || i+36 > len(b) {
			continue
		}
		c := b[i+4:]
		var scale, dur int64
		if c[0] == 1 {
			scale, dur = be32(c[20:24]), int64(binary.BigEndian.Uint64(c[24:32])&math.MaxInt64)
		} else {
			scale, dur = be32(c[12:16]), be32(c[16:20])
		}
		if scale > 0 && dur > 0 && dur != math.MaxUint32 {
			return int64(float64(dur) * 1000 / float64(scale))
		}
	}
	return 0
}

func webmMs(h []byte) int64 {
	if len(h) < 4 || !bytes.Equal(h[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return 0
	}
	scale := 1_000_000.0 // TimecodeScale default: 1 ms in ns
	if i := bytes.Index(h, []byte{0x2a, 0xd7, 0xb1}); i >= 0 && i+4 < len(h) {
		if n := int(h[i+3] &^ 0x80); h[i+3]&0x80 != 0 && n >= 1 && n <= 8 && i+4+n <= len(h) {
			v := int64(0)
			for _, x := range h[i+4 : i+4+n] {
				v = v<<8 | int64(x)
			}
			if v > 0 {
				scale = float64(v)
			}
		}
	}
	i := bytes.Index(h, []byte{0x44, 0x89})
	if i < 0 || i+3 > len(h) {
		return 0
	}
	var d float64
	switch v := h[i+3:]; h[i+2] {
	case 0x84:
		if len(v) >= 4 {
			d = float64(math.Float32frombits(binary.BigEndian.Uint32(v)))
		}
	case 0x88:
		if len(v) >= 8 {
			d = math.Float64frombits(binary.BigEndian.Uint64(v))
		}
	}
	if d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return 0
	}
	return int64(d * scale / 1e6)
}

var (
	mp3Kbps = [2][3][15]int64{
		{ // MPEG-1: layer I, II, III
			{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
			{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
		},
		{ // MPEG-2 and 2.5
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		},
	}
	mp3Rates = map[byte][3]int64{3: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 0: {11025, 12000, 8000}}
)

// mp3Ms reads the first frame: a Xing/Info or VBRI frame count when
// present, else the bitrate as constant.
func mp3Ms(h []byte, size int64) int64 {
	off := 0
	if len(h) >= 10 && string(h[:3]) == "ID3" {
		off = 10 + (int(h[6]&0x7f)<<21 | int(h[7]&0x7f)<<14 | int(h[8]&0x7f)<<7 | int(h[9]&0x7f))
		if h[5]&0x10 != 0 {
			off += 10
		}
	}
	for end := min(len(h)-4, off+4096); off < end; off++ {
		if h[off] != 0xff || h[off+1]&0xe0 != 0xe0 {
			continue
		}
		ver, layer := (h[off+1]>>3)&3, (h[off+1]>>1)&3
		bi, si := h[off+2]>>4, (h[off+2]>>2)&3
		if ver == 1 || layer == 0 || bi == 0 || bi == 15 || si == 3 {
			continue
		}
		v := 0
		if ver != 3 {
			v = 1
		}
		kbps, rate := mp3Kbps[v][3-layer][bi], mp3Rates[ver][si]
		spf := int64(1152)
		switch {
		case layer == 3:
			spf = 384
		case layer == 1 && v == 1:
			spf = 576
		}
		side := 32
		switch mono := h[off+3]>>6 == 3; {
		case v == 0 && mono:
			side = 17
		case v == 1 && mono:
			side = 9
		case v == 1:
			side = 17
		}
		if x := off + 4 + side; x+12 <= len(h) && (string(h[x:x+4]) == "Xing" || string(h[x:x+4]) == "Info") && h[x+7]&1 != 0 {
			if frames := be32(h[x+8 : x+12]); frames > 0 {
				return frames * spf * 1000 / rate
			}
		}
		if x := off + 36; x+18 <= len(h) && string(h[x:x+4]) == "VBRI" {
			if frames := be32(h[x+14 : x+18]); frames > 0 {
				return frames * spf * 1000 / rate
			}
		}
		return (size - int64(off)) * 8 / kbps
	}
	return 0
}
