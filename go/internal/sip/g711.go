package sip

// G.711 A-law <-> μ-law transcoding via 16-bit linear PCM.
//
// Used in AUDIO_MODE=twilio when the caller does not offer PCMU: the SIP side
// then negotiates PCMA (RFC 3264 §6.1: a sendrecv answer must contain at least
// one codec from the offer, and the answerer must send using a format listed
// in both), and the WebSocket side keeps the Twilio contract of audio/x-mulaw.
//
// The two 256-entry lookup tables are built once at init: each code is
// decoded to 16-bit linear PCM and re-encoded with the standard G.711
// segment encoder (the same algorithm as the widely used Sun Microsystems
// reference g711.c). Per-frame conversion is then a table lookup per byte.

var (
	alawToULaw [256]byte
	ulawToALaw [256]byte
)

func alawToLinear(a byte) int {
	a ^= 0x55
	t := int(a&0x0F) << 4
	seg := int(a&0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= uint(seg - 1)
	}
	if a&0x80 != 0 {
		return t
	}
	return -t
}

func ulawToLinear(u byte) int {
	u = ^u
	t := (int(u&0x0F) << 3) + 0x84
	t <<= uint(int(u&0x70) >> 4)
	if u&0x80 != 0 {
		return 0x84 - t
	}
	return t - 0x84
}

func segment(v int, ends []int) int {
	for i, e := range ends {
		if v <= e {
			return i
		}
	}
	return len(ends)
}

var (
	segAEnd = []int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}
	segUEnd = []int{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}
)

func linearToALaw(pcm int) byte {
	pcm >>= 3
	mask := 0xD5
	if pcm < 0 {
		mask = 0x55
		pcm = -pcm - 1
	}
	seg := segment(pcm, segAEnd)
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	aval := seg << 4
	if seg < 2 {
		aval |= (pcm >> 1) & 0x0F
	} else {
		aval |= (pcm >> uint(seg)) & 0x0F
	}
	return byte(aval ^ mask)
}

func linearToULaw(pcm int) byte {
	pcm >>= 2
	mask := 0xFF
	if pcm < 0 {
		pcm = -pcm
		mask = 0x7F
	}
	if pcm > 8159 {
		pcm = 8159
	}
	pcm += 0x84 >> 2
	seg := segment(pcm, segUEnd)
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	uval := (seg << 4) | ((pcm >> uint(seg+1)) & 0x0F)
	return byte(uval ^ mask)
}

func init() {
	for i := 0; i < 256; i++ {
		alawToULaw[i] = linearToULaw(alawToLinear(byte(i)))
		ulawToALaw[i] = linearToALaw(ulawToLinear(byte(i)))
	}
}

// ALawToULaw converts a PCMA payload to PCMU. A new slice is returned.
func ALawToULaw(p []byte) []byte {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = alawToULaw[b]
	}
	return out
}

// ULawToALaw converts a PCMU payload to PCMA. A new slice is returned.
func ULawToALaw(p []byte) []byte {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = ulawToALaw[b]
	}
	return out
}
