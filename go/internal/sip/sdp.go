package sip

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"
)

// CallerSDP holds the fields extracted from the caller's SDP offer.
type CallerSDP struct {
	RTPAddr           string  // IP address to send RTP to (caller's media IP)
	RTPPort           int     // UDP port to send RTP to
	DTMFPayloadType   uint8   // telephone-event PT (sipgate uses 113, not the conventional 101)
	AudioPayloadTypes []uint8 // all non-DTMF audio PTs from offer, in order
	// SRTP fields — populated when the offer uses RTP/SAVP with SDES crypto (RFC 4568).
	IsSRTP         bool   // true when offer uses RTP/SAVP protocol
	RemoteSRTPKey  []byte // 16-byte AES-128 master key from caller's a=crypto: line
	RemoteSRTPSalt []byte // 14-byte master salt from caller's a=crypto: line
}

// ParseCallerSDP extracts the caller's RTP destination and DTMF payload type from an SDP offer.
// CRITICAL: DTMF PT is NEVER hardcoded — always extracted from the SDP offer.
// When the offer uses RTP/SAVP, the IsSRTP flag is set and the remote SRTP master key/salt are
// extracted from the first AES_128_CM_HMAC_SHA1_80 a=crypto: line (RFC 4568).
func ParseCallerSDP(body []byte) (*CallerSDP, error) {
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal(body); err != nil {
		return nil, fmt.Errorf("SDP unmarshal: %w", err)
	}

	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Media != "audio" {
			continue
		}
		port := md.MediaName.Port.Value

		// Connection address: per-media preferred over session-level
		ip := ""
		if md.ConnectionInformation != nil {
			ip = md.ConnectionInformation.Address.Address
		} else if sd.ConnectionInformation != nil {
			ip = sd.ConnectionInformation.Address.Address
		}
		if ip == "" {
			return nil, fmt.Errorf("no connection address in SDP")
		}

		// Scan ALL format entries: separate telephone-event from audio PTs.
		var dtmfPT uint8 = 101 // fallback to conventional value if not found
		var audioPTs []uint8
		for _, fmtStr := range md.MediaName.Formats {
			pt, err := strconv.ParseUint(fmtStr, 10, 8)
			if err != nil {
				continue
			}
			codec, err := sd.GetCodecForPayloadType(uint8(pt))
			if err != nil {
				continue
			}
			if strings.EqualFold(codec.Name, "telephone-event") {
				dtmfPT = uint8(pt) // last telephone-event wins (prefer 8kHz over 48kHz)
			} else {
				audioPTs = append(audioPTs, uint8(pt))
			}
		}

		// Detect RTP/SAVP protocol (SRTP offer per RFC 4568).
		isSRTP := false
		for _, proto := range md.MediaName.Protos {
			if strings.EqualFold(proto, "SAVP") {
				isSRTP = true
				break
			}
		}

		// Extract remote SRTP master key+salt from a=crypto: attribute when present.
		// Only AES_128_CM_HMAC_SHA1_80 is supported (the most common SDES suite).
		// key+salt = 30 bytes (16-byte key + 14-byte salt), base64-encoded (40 chars).
		var remoteSRTPKey, remoteSRTPSalt []byte
		for _, attr := range md.Attributes {
			if attr.Key != "crypto" {
				continue
			}
			// Format: <tag> AES_128_CM_HMAC_SHA1_80 inline:<base64keyAndSalt>[|<params>...]
			parts := strings.Fields(attr.Value)
			if len(parts) < 3 {
				continue
			}
			if !strings.EqualFold(parts[1], "AES_128_CM_HMAC_SHA1_80") {
				continue
			}
			// Strip "inline:" prefix
			inlineVal := strings.TrimPrefix(parts[2], "inline:")
			// base64 may have trailing session params after "|" or " "
			inlineVal = strings.SplitN(inlineVal, "|", 2)[0]
			keyAndSalt, err := base64.StdEncoding.DecodeString(inlineVal)
			if err != nil || len(keyAndSalt) < 30 {
				continue
			}
			remoteSRTPKey = keyAndSalt[:16]
			remoteSRTPSalt = keyAndSalt[16:30]
			break
		}

		return &CallerSDP{
			RTPAddr:           ip,
			RTPPort:           port,
			DTMFPayloadType:   dtmfPT,
			AudioPayloadTypes: audioPTs,
			IsSRTP:            isSRTP,
			RemoteSRTPKey:     remoteSRTPKey,
			RemoteSRTPSalt:    remoteSRTPSalt,
		}, nil
	}
	return nil, fmt.Errorf("no audio media section in SDP offer")
}

// SilenceFrameForPT returns a 160-byte silence frame for the given RTP payload type.
// Exported so bridge.manager can call it without importing a silence constant directly.
func SilenceFrameForPT(pt uint8) []byte {
	b := make([]byte, 160)
	var silenceByte byte
	switch pt {
	case 9: // G.722: ADPCM silence is 0x00
		silenceByte = 0x00
	case 8: // PCMA: A-law silence is 0xD5
		silenceByte = 0xD5
	default: // PCMU (PT 0): μ-law silence is 0xFF
		silenceByte = 0xFF
	}
	for i := range b {
		b[i] = silenceByte
	}
	return b
}

// MediaFormatForPT returns the Twilio mediaFormat encoding and sample rate for a payload type.
func MediaFormatForPT(pt uint8) (encoding string, sampleRate int) {
	switch pt {
	case 9:
		return "audio/G722", 16000
	case 8:
		return "audio/x-alaw", 8000
	default: // PCMU (PT 0)
		return "audio/x-mulaw", 8000
	}
}

// codecEntry pairs a payload type with its codec name for SDP generation.
type codecEntry struct {
	pt   uint8
	name string
}

// BuildSDPAnswer constructs an SDP answer based on the caller's offer and the configured audio mode.
// Returns the SDP bytes, the negotiated audio payload type, and (when SRTP is negotiated) the
// local SRTP master key+salt that were generated for this session.
//
// srtpEnabled controls whether we negotiate SRTP: when true and the caller's offer uses
// RTP/SAVP, the answer uses RTP/SAVP with an a=crypto: line (SDES, RFC 4568).
// When the offer is plain RTP/AVP, or srtpEnabled is false, the answer uses RTP/AVP.
//
// ourIP is cfg.SDPContactIP (externally reachable host IP — not the container IP).
func BuildSDPAnswer(ourIP string, ourRTPPort int, callerSDP *CallerSDP, audioMode string, srtpEnabled bool) (sdpBytes []byte, negotiatedPT uint8, localSRTPKey []byte, localSRTPSalt []byte) {
	now := uint64(time.Now().UnixNano())
	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      now,
			SessionVersion: now,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: ourIP,
		},
		SessionName: sdp.SessionName("sipgate-sip-stream-bridge"),
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: ourIP},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
	}

	dtmfPTStr := strconv.FormatUint(uint64(callerSDP.DTMFPayloadType), 10)

	// Determine which audio codecs to advertise and the negotiated PT.
	var codecs []codecEntry
	if audioMode == "best" {
		// Preferred order: G.722 > PCMA > PCMU — filtered to what caller offered.
		for _, c := range []codecEntry{{9, "G722"}, {8, "PCMA"}, {0, "PCMU"}} {
			for _, offered := range callerSDP.AudioPayloadTypes {
				if offered == c.pt {
					codecs = append(codecs, c)
					break
				}
			}
		}
	} else {
		// twilio mode: always PCMU regardless of offer
		codecs = []codecEntry{{0, "PCMU"}}
	}

	if len(codecs) > 0 {
		negotiatedPT = codecs[0].pt
	} // else negotiatedPT stays 0 (PCMU fallback)

	// Build format list: audio codecs first, then DTMF.
	formats := make([]string, 0, len(codecs)+1)
	for _, c := range codecs {
		formats = append(formats, strconv.FormatUint(uint64(c.pt), 10))
	}
	formats = append(formats, dtmfPTStr)

	// Negotiate SRTP (RTP/SAVP) when enabled and the caller's offer is also SAVP.
	// When the offer is plain AVP, or SRTP is disabled, fall back to RTP/AVP.
	useSRTP := srtpEnabled && callerSDP.IsSRTP

	proto := []string{"RTP", "AVP"}
	if useSRTP {
		proto = []string{"RTP", "SAVP"}
	}

	audio := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: ourRTPPort},
			Protos:  proto,
			Formats: formats,
		},
	}

	// When SRTP is negotiated, generate a random local master key+salt and add a=crypto:.
	if useSRTP {
		key := make([]byte, 16)
		salt := make([]byte, 14)
		if _, err := rand.Read(key); err == nil {
			if _, err := rand.Read(salt); err == nil {
				localSRTPKey = key
				localSRTPSalt = salt
				keyAndSalt := append(key, salt...)
				cryptoVal := fmt.Sprintf("1 AES_128_CM_HMAC_SHA1_80 inline:%s",
					base64.StdEncoding.EncodeToString(keyAndSalt))
				audio.Attributes = append(audio.Attributes, sdp.Attribute{Key: "crypto", Value: cryptoVal})
			}
		}
	}

	// Add rtpmap for each advertised codec.
	for _, c := range codecs {
		audio.WithCodec(c.pt, c.name, 8000, 1, "")
	}
	audio.WithCodec(callerSDP.DTMFPayloadType, "telephone-event", 8000, 1, "0-16")
	audio.WithPropertyAttribute("sendrecv")

	sd.MediaDescriptions = append(sd.MediaDescriptions, audio)

	out, _ := sd.Marshal()
	return out, negotiatedPT, localSRTPKey, localSRTPSalt
}
