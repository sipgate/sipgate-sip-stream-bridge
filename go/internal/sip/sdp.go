package sip

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"
)

// CallerSDP holds the fields extracted from the caller's SDP offer.
type CallerSDP struct {
	RTPAddr         string // IP address to send RTP to (caller's media IP)
	RTPPort         int    // UDP port to send RTP to
	DTMFPayloadType uint8  // telephone-event PT (sipgate uses 113, not the conventional 101)
}

// ParseCallerSDP extracts the caller's RTP destination and DTMF payload type from an SDP offer.
// CRITICAL: DTMF PT is NEVER hardcoded — always extracted from SDP offer per STATE.md decision.
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

		// Find telephone-event PT — scan ALL format entries for rtpmap
		var dtmfPT uint8 = 101 // fallback to conventional value if not found
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
				dtmfPT = uint8(pt) // may be 113 (sipgate) or 101 (conventional)
			}
		}

		return &CallerSDP{
			RTPAddr:         ip,
			RTPPort:         port,
			DTMFPayloadType: dtmfPT,
		}, nil
	}
	return nil, fmt.Errorf("no audio media section in SDP offer")
}

// BuildSDPAnswer constructs an SDP answer advertising PCMU (PT 0) + telephone-event.
// ourIP is cfg.SDPContactIP (externally reachable host IP — not the container IP).
// callerDTMFPT is mirrored from the caller's offer (SDP negotiation rule: answer must mirror PT).
func BuildSDPAnswer(ourIP string, ourRTPPort int, callerDTMFPT uint8) []byte {
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
		SessionName: sdp.SessionName("audio-dock"),
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: ourIP},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
	}

	dtmfPTStr := strconv.FormatUint(uint64(callerDTMFPT), 10)
	audio := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: ourRTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: []string{"0", dtmfPTStr},
		},
	}
	audio.WithCodec(0, "PCMU", 8000, 1, "")
	audio.WithCodec(callerDTMFPT, "telephone-event", 8000, 1, "0-16")
	audio.WithPropertyAttribute("sendrecv")

	sd.MediaDescriptions = append(sd.MediaDescriptions, audio)

	out, _ := sd.Marshal()
	return out
}
