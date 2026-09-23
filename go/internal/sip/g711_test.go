package sip

import "testing"

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Silence must stay silence across the conversion - otherwise the caller hears
// a constant hiss whenever the bot is quiet.
func TestG711SilenceStaysSilent(t *testing.T) {
	if v := ulawToLinear(ALawToULaw([]byte{0xD5})[0]); abs(v) > 16 {
		t.Fatalf("A-law silence 0xD5 became μ-law with linear value %d", v)
	}
	if v := alawToLinear(ULawToALaw([]byte{0xFF})[0]); abs(v) > 16 {
		t.Fatalf("μ-law silence 0xFF became A-law with linear value %d", v)
	}
}

// Every code must land near its own level, with the sign kept. The bound is
// the coarser of the two quantisation steps at that level.
func TestG711ConversionKeepsLevelAndSign(t *testing.T) {
	for i := 0; i < 256; i++ {
		a := alawToLinear(byte(i))
		u := ulawToLinear(ALawToULaw([]byte{byte(i)})[0])
		if (a > 16 && u < 0) || (a < -16 && u > 0) {
			t.Fatalf("A-law 0x%02X: sign flipped (%d -> %d)", i, a, u)
		}
		if d := abs(a - u); d > abs(a)/8+24 {
			t.Fatalf("A-law 0x%02X: level %d became %d (diff %d)", i, a, u, d)
		}
		m := ulawToLinear(byte(i))
		b := alawToLinear(ULawToALaw([]byte{byte(i)})[0])
		if (m > 16 && b < 0) || (m < -16 && b > 0) {
			t.Fatalf("μ-law 0x%02X: sign flipped (%d -> %d)", i, m, b)
		}
		if d := abs(m - b); d > abs(m)/8+24 {
			t.Fatalf("μ-law 0x%02X: level %d became %d (diff %d)", i, m, b, d)
		}
	}
}

// The reference decoders must span the 16-bit range - a wrong segment shift
// would squash loud speech.
func TestG711FullScale(t *testing.T) {
	if alawToLinear(0xAA) < 30000 || alawToLinear(0x2A) > -30000 {
		t.Fatalf("A-law full scale wrong: %d / %d", alawToLinear(0xAA), alawToLinear(0x2A))
	}
	if ulawToLinear(0x80) < 30000 || ulawToLinear(0x00) > -30000 {
		t.Fatalf("μ-law full scale wrong: %d / %d", ulawToLinear(0x80), ulawToLinear(0x00))
	}
}

func TestG711ReturnsNewSlice(t *testing.T) {
	in := []byte{0xD5, 0x55}
	out := ALawToULaw(in)
	if &out[0] == &in[0] {
		t.Fatal("ALawToULaw returned its input slice")
	}
	if in[0] != 0xD5 || in[1] != 0x55 {
		t.Fatalf("ALawToULaw modified its input: % X", in)
	}

	in = []byte{0xFF, 0x7F}
	out = ULawToALaw(in)
	if &out[0] == &in[0] {
		t.Fatal("ULawToALaw returned its input slice")
	}
	if in[0] != 0xFF || in[1] != 0x7F {
		t.Fatalf("ULawToALaw modified its input: % X", in)
	}
}

// Exact decoder values, one code per exponent (3-bit segment field) plus the
// negative end points. Reference: CPython's audioop.alaw2lin / ulaw2lin,
// cross-checked against ffmpeg's pcm_alaw / pcm_mulaw decoders; the two agree
// with each other, and with these decoders, on all 256 codes of both laws.
func TestG711DecodeReference(t *testing.T) {
	alaw := []struct {
		code byte
		want int
	}{
		{0xD5, 8}, // exponent 0, mantissa 0 (silence)
		{0xDA, 248},
		{0xC5, 264}, // exponent 1, mantissa 0
		{0xCA, 504},
		{0xF2, 752},
		{0xE6, 1248},
		{0x90, 2752},
		{0x8F, 6784},
		{0xB4, 8960},
		{0xAA, 32256}, // exponent 7, mantissa 15 (full scale)
		{0x55, -8},
		{0x2A, -32256},
	}
	for _, c := range alaw {
		if got := alawToLinear(c.code); got != c.want {
			t.Errorf("alawToLinear(0x%02X) = %d, want %d", c.code, got, c.want)
		}
	}

	ulaw := []struct {
		code byte
		want int
	}{
		{0xFF, 0}, // exponent 0, mantissa 0 (silence)
		{0xFE, 8},
		{0xF0, 120},
		{0xEF, 132}, // exponent 1, mantissa 0
		{0xE0, 372},
		{0xD8, 620},
		{0xCC, 1116},
		{0xBA, 2620},
		{0xA5, 6652},
		{0x9E, 8828},
		{0x80, 32124}, // exponent 7, mantissa 15 (full scale)
		{0x7F, 0},     // negative zero
		{0x00, -32124},
	}
	for _, c := range ulaw {
		if got := ulawToLinear(c.code); got != c.want {
			t.Errorf("ulawToLinear(0x%02X) = %d, want %d", c.code, got, c.want)
		}
	}
}

// Exact conversion pairs. Reference: CPython audioop, lin2ulaw(alaw2lin(x))
// and lin2alaw(ulaw2lin(x)), which matches these tables on all 256 codes.
// ffmpeg's pcm_alaw <-> pcm_mulaw conversion rounds a few codes differently,
// but gives the same result for every pair listed here.
func TestG711ConversionReference(t *testing.T) {
	a2u := []struct{ in, want byte }{
		{0xD5, 0xFE}, // silence
		{0x55, 0x7E}, // negative silence
		{0xAA, 0x80}, // full scale
		{0x2A, 0x00}, // negative full scale
		{0xC5, 0xE7}, // exponent 1
		{0x90, 0xB9}, // exponent 4
	}
	for _, c := range a2u {
		if got := ALawToULaw([]byte{c.in})[0]; got != c.want {
			t.Errorf("ALawToULaw(0x%02X) = 0x%02X, want 0x%02X", c.in, got, c.want)
		}
	}

	u2a := []struct{ in, want byte }{
		{0xFF, 0xD5}, // silence
		{0x7F, 0xD5}, // negative zero decodes to 0
		{0x80, 0xAA}, // full scale
		{0x00, 0x2A}, // negative full scale
		{0xEF, 0xDD}, // exponent 1
		{0xBA, 0x91}, // exponent 4
	}
	for _, c := range u2a {
		if got := ULawToALaw([]byte{c.in})[0]; got != c.want {
			t.Errorf("ULawToALaw(0x%02X) = 0x%02X, want 0x%02X", c.in, got, c.want)
		}
	}
}

// Decoder and encoder must be inverse on every code: encode(decode(x)) == x.
// The one exception is μ-law 0x7F (negative zero), which decodes to 0 and
// re-encodes as 0xFF.
func TestG711RoundTrip(t *testing.T) {
	for i := 0; i < 256; i++ {
		x := byte(i)
		if got := linearToALaw(alawToLinear(x)); got != x {
			t.Errorf("A-law 0x%02X: decode %d, re-encoded as 0x%02X", x, alawToLinear(x), got)
		}
		want := x
		if x == 0x7F {
			want = 0xFF
		}
		if got := linearToULaw(ulawToLinear(x)); got != want {
			t.Errorf("μ-law 0x%02X: decode %d, re-encoded as 0x%02X, want 0x%02X", x, ulawToLinear(x), got, want)
		}
	}
}
