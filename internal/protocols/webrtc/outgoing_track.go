package webrtc

import (
	"fmt"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpsender"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// OutgoingTrack is a WebRTC outgoing track
type OutgoingTrack struct {
	Caps webrtc.RTPCodecCapability

	track      *webrtc.TrackLocalStaticRTP
	sender     *webrtc.RTPSender
	ssrc       uint32
	rtcpSender *rtpsender.Sender

	// Simulcast support
	simulcastEncodings []webrtc.RTPEncodingParameters
	ridToSSRC          map[string]uint32 // RID -> SSRC mapping
}

func (t *OutgoingTrack) isVideo() bool {
	return strings.Split(t.Caps.MimeType, "/")[0] == "video"
}

// Setup sets up the track with a PeerConnection.
// This is a public wrapper around the private setup method.
func (t *OutgoingTrack) Setup(p *PeerConnection) error {
	return t.setup(p)
}

func (t *OutgoingTrack) setup(p *PeerConnection) error {
	var trackID string
	if t.isVideo() {
		trackID = "video"
	} else {
		trackID = "audio"
	}

	var err error
	t.track, err = webrtc.NewTrackLocalStaticRTP(
		t.Caps,
		trackID,
		webrtcStreamID,
	)
	if err != nil {
		return err
	}

	sender, err := p.wr.AddTrack(t.track)
	if err != nil {
		return err
	}

	t.sender = sender
	t.ssrc = uint32(sender.GetParameters().Encodings[0].SSRC)

	t.rtcpSender = &rtpsender.Sender{
		ClockRate: int(t.track.Codec().ClockRate),
		Period:    1 * time.Second,
		TimeNow:   time.Now,
		WritePacketRTCP: func(pkt rtcp.Packet) {
			p.wr.WriteRTCP([]rtcp.Packet{pkt}) //nolint:errcheck
		},
	}
	t.rtcpSender.Initialize()

	// incoming RTCP packets must always be read to make interceptors work
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err2 := sender.Read(buf)
			if err2 != nil {
				return
			}

			_, err2 = rtcp.Unmarshal(buf[:n])
			if err2 != nil {
				panic(err2)
			}
		}
	}()

	return nil
}

func (t *OutgoingTrack) close() {
	if t.rtcpSender != nil {
		t.rtcpSender.Close()
	}
}

// WriteRTP writes a RTP packet.
func (t *OutgoingTrack) WriteRTP(pkt *rtp.Packet) error {
	return t.WriteRTPWithNTP(pkt, time.Now())
}

// WriteRTPWithNTP writes a RTP packet.
func (t *OutgoingTrack) WriteRTPWithNTP(pkt *rtp.Packet, ntp time.Time) error {
	return t.WriteRTPWithRID(pkt, ntp, "")
}

// WriteRTPWithRID writes a RTP packet with a specific RID (for simulcast).
func (t *OutgoingTrack) WriteRTPWithRID(pkt *rtp.Packet, ntp time.Time, rid string) error {
	// Determine SSRC based on RID if simulcast is enabled
	if rid != "" && t.ridToSSRC != nil {
		if ssrc, ok := t.GetSSRCForRID(rid); ok {
			pkt.SSRC = ssrc
		} else {
			// Fallback to primary SSRC if RID not found
			pkt.SSRC = t.ssrc
		}
	} else {
		// use right SSRC in packet to make rtcpSender work
		pkt.SSRC = t.ssrc
	}

	// rtcpSender may be nil if setup() hasn't been called yet
	// This can happen when tracks are created before PeerConnection.Start()
	if t.rtcpSender != nil {
		t.rtcpSender.ProcessPacket(pkt, ntp, true)
	}

	// track may be nil if setup() hasn't been called yet
	if t.track != nil {
		return t.track.WriteRTP(pkt)
	}

	return nil
}

// ConfigureSimulcast configures Simulcast encodings for this track.
// This method stores the encodings for later use when writing RTP packets and SDP generation.
func (t *OutgoingTrack) ConfigureSimulcast(encodings []webrtc.RTPEncodingParameters) error {
	if len(encodings) == 0 {
		return fmt.Errorf("encodings cannot be empty")
	}

	// Store encodings for later use
	t.simulcastEncodings = encodings

	// Create RID to SSRC mapping
	t.ridToSSRC = make(map[string]uint32)
	for _, enc := range encodings {
		if enc.RID != "" {
			t.ridToSSRC[enc.RID] = uint32(enc.SSRC)
		}
	}

	// Update SSRC to use the first encoding's SSRC as the primary SSRC
	if len(encodings) > 0 {
		t.ssrc = uint32(encodings[0].SSRC)
	}

	return nil
}

// GetSimulcastEncodings returns the simulcast encodings for this track.
func (t *OutgoingTrack) GetSimulcastEncodings() []webrtc.RTPEncodingParameters {
	return t.simulcastEncodings
}

// GetSSRCForRID returns the SSRC for a given RID.
func (t *OutgoingTrack) GetSSRCForRID(rid string) (uint32, bool) {
	if t.ridToSSRC == nil {
		return 0, false
	}
	ssrc, ok := t.ridToSSRC[rid]
	return ssrc, ok
}

// GetSender returns the RTPSender (for debugging purposes).
func (t *OutgoingTrack) GetSender() *webrtc.RTPSender {
	return t.sender
}
