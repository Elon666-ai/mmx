// Copyright (C) 2024 MediaMTX
// SPDX-License-Identifier: MIT
//
// ABR Track Switcher for MediaMTX - Enhanced with Audio Support
// Handles dynamic switching between video quality tracks and audio-only mode

package webrtc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpvp8"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpvp9"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	wsprotocol "github.com/bluenviron/mediamtx/internal/protocols/websocket"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TrackSwitcher manages dynamic switching between different quality tracks for ABR
// Enhanced to support audio-only mode for low bandwidth scenarios
type TrackSwitcher struct {
	mu              sync.RWMutex
	currentTrackIdx int
	tracks          []*QualityTrack
	audioTrackIdx   int            // Index of audio track in tracks array (-1 if none)
	outgoingTrack   *OutgoingTrack // The single WebRTC outgoing track
	enabled         atomic.Bool

	// Sequence number and timestamp continuity
	lastSeqNum      uint16
	lastTimestamp   uint32
	timestampOffset int64
	seqNumOffset    int32
	continuityInit  bool

	// Expected values for next track after switch
	expectedNextSeq uint16
	expectedNextTs  uint32
	expectingSwitch bool // True when waiting for first packet after switch

	// Track switch coordination
	switchInProgress atomic.Bool
	audioOnlyMode    atomic.Bool // True when in audio-only mode

	logger logger.Writer
	ctx    context.Context
}

// QualityTrack represents a single quality level with its media source
type QualityTrack struct {
	ID     int
	Type   string // "video" or "audio"
	Label  string // "High", "Medium", "Low", "Audio"
	Media  *description.Media
	Format format.Format

	// Track metadata
	Bitrate int
	Width   int
	Height  int
	Codec   string

	// Encoder for this track
	encoder interface{} // Type depends on codec (rtph264.Encoder, etc.)

	// Packet handling
	active             atomic.Bool
	lastPTS            int64
	firstPacket        bool
	waitingForKeyframe bool // Wait for keyframe after track switch to avoid glitches

	// Reference to the outgoing track and logger
	outgoingTrack *OutgoingTrack
	switcher      *TrackSwitcher
}

// Helper functions for timestamp conversion
func multiplyAndDivide2(v, m, d time.Duration) time.Duration {
	secs := v / d
	dec := v % d
	return (secs*m + dec*m/d)
}

func timestampToDuration(t int64, clockRate int) time.Duration {
	return multiplyAndDivide2(time.Duration(t), time.Second, time.Duration(clockRate))
}

// isKeyframe detects if a video frame is a keyframe (I-frame/IDR)
// This is used to ensure smooth track switching without visual glitches
func isKeyframe(u *unit.Unit, codec string) bool {
	switch codec {
	case "H264":
		payload := u.Payload.(unit.PayloadH264)
		for _, nalu := range payload {
			if len(nalu) > 0 {
				nalType := nalu[0] & 0x1F
				if nalType == 5 { // IDR frame (keyframe)
					return true
				}
			}
		}
		return false

	case "H265":
		payload := u.Payload.(unit.PayloadH265)
		for _, nalu := range payload {
			if len(nalu) > 1 {
				nalType := (nalu[0] >> 1) & 0x3F
				// IDR, CRA, or BLA frames are keyframes
				if nalType >= 16 && nalType <= 21 {
					return true
				}
			}
		}
		return false

	case "VP8", "VP9":
		// For VP8/VP9, simplified implementation
		// In production, should check partition headers
		return true

	default:
		return true
	}
}

// NewTrackSwitcher creates a new track switcher
func NewTrackSwitcher(ctx context.Context, log logger.Writer) *TrackSwitcher {
	ts := &TrackSwitcher{
		currentTrackIdx: 0,
		audioTrackIdx:   -1, // No audio track initially
		tracks:          make([]*QualityTrack, 0),
		logger:          log,
		ctx:             ctx,
		continuityInit:  false,
	}
	ts.enabled.Store(true)
	ts.switchInProgress.Store(false)
	ts.audioOnlyMode.Store(false)
	return ts
}

// SetupFromStream sets up multiple quality tracks from a stream description
// Now includes audio track setup with ID as the last track
func (ts *TrackSwitcher) SetupFromStream(
	desc *description.Session,
	r *stream.Reader,
	pc *PeerConnection,
	pathConf interface{ SafeConf() *conf.Path },
) error {
	// Find video format for the outgoing track
	var mainFormat format.Format
	for _, media := range desc.Medias {
		if media.Type == description.MediaTypeVideo && len(media.Formats) > 0 {
			mainFormat = media.Formats[0]
			break
		}
	}

	if mainFormat == nil {
		return fmt.Errorf("no video format found in stream")
	}

	// Create a single outgoing WebRTC track (the client sees one track)
	// Determine codec from format
	codecMime := ""
	clockRate := uint32(90000) // Standard for video

	switch mainFormat.Codec() {
	case "H264":
		codecMime = webrtc.MimeTypeH264
	case "H265":
		codecMime = webrtc.MimeTypeH265
	case "VP8":
		codecMime = webrtc.MimeTypeVP8
	case "VP9":
		codecMime = webrtc.MimeTypeVP9
	default:
		return fmt.Errorf("unsupported codec: %s", mainFormat.Codec())
	}

	ts.outgoingTrack = &OutgoingTrack{
		Caps: webrtc.RTPCodecCapability{
			MimeType:  codecMime,
			ClockRate: clockRate,
		},
	}
	pc.OutgoingTracks = append(pc.OutgoingTracks, ts.outgoingTrack)
	ts.logger.Log(logger.Info, "[Track Switcher] Created outgoing track: %s @ %dHz", codecMime, clockRate)

	// Now set up video quality tracks from all video media in the description
	trackIdx := 0
	for _, media := range desc.Medias {
		if media.Type != description.MediaTypeVideo {
			continue
		}

		for _, mediaFormat := range media.Formats {
			// Create a quality track for each format
			track := &QualityTrack{
				ID:            trackIdx,
				Type:          "video",
				Media:         media,
				Format:        mediaFormat,
				firstPacket:   true,
				outgoingTrack: ts.outgoingTrack,
				switcher:      ts,
			}

			// Assign quality labels based on track order
			switch trackIdx {
			case 0:
				track.Label = "High"
				track.Bitrate = 2000000
				track.Width = 1920
				track.Height = 1080
			case 1:
				track.Label = "Medium"
				track.Bitrate = 1000000
				track.Width = 1280
				track.Height = 720
			case 2:
				track.Label = "Low"
				track.Bitrate = 400000
				track.Width = 960
				track.Height = 540
			default:
				track.Label = fmt.Sprintf("Track%d", trackIdx)
				track.Bitrate = 500000
				track.Width = 320
				track.Height = 180
			}

			// Determine codec
			track.Codec = mediaFormat.Codec()

			// Set up encoder based on format
			if err := ts.setupEncoder(track); err != nil {
				return fmt.Errorf("failed to setup encoder for track %d: %w", trackIdx, err)
			}

			// Register data callback for this quality track
			if err := ts.registerTrackCallback(track, r); err != nil {
				return fmt.Errorf("failed to register callback for track %d: %w", trackIdx, err)
			}

			ts.tracks = append(ts.tracks, track)
			ts.logger.Log(logger.Info, "[Track Switcher] Setup quality track %d: %s (%s, %dx%d, %d bps)",
				trackIdx, track.Label, track.Codec, track.Width, track.Height, track.Bitrate)

			trackIdx++
		}
	}

	if len(ts.tracks) == 0 {
		return fmt.Errorf("no video tracks found in stream")
	}

	// ✅ NEW: Add audio track as the last track
	audioTrackIdx := ts.setupAudioTrack(desc, r, pc)
	if audioTrackIdx >= 0 {
		ts.audioTrackIdx = audioTrackIdx
		ts.logger.Log(logger.Info, "[Track Switcher] Audio track added with ID: %d", audioTrackIdx)
	} else {
		ts.logger.Log(logger.Warn, "[Track Switcher] No audio track found in stream")
	}

	// Activate the first video track by default
	ts.tracks[0].active.Store(true)
	ts.logger.Log(logger.Info, "[Track Switcher] Initialized with %d total tracks (%d video + %d audio), active: 0 (%s)",
		len(ts.tracks), len(ts.tracks)-1, 1, ts.tracks[0].Label)

	return nil
}

// setupAudioTrack sets up the audio track with full data streaming support
// Returns the track index, or -1 if no audio found
func (ts *TrackSwitcher) setupAudioTrack(
	desc *description.Session,
	r *stream.Reader,
	pc *PeerConnection,
) int {
	// Find audio media in description
	for _, media := range desc.Medias {
		if media.Type != description.MediaTypeAudio {
			continue
		}

		if len(media.Formats) == 0 {
			continue
		}

		// Use the first audio format
		audioFormat := media.Formats[0]
		audioTrackID := len(ts.tracks) // Audio track gets next available ID

		// Check if it's Opus format (most common for WebRTC)
		opusFormat, isOpus := audioFormat.(*format.Opus)
		if !isOpus {
			ts.logger.Log(logger.Warn, "[Track Switcher] Unsupported audio format: %s (only Opus is supported)", audioFormat.Codec())
			return -1
		}

		ts.logger.Log(logger.Info, "[Track Switcher] Found audio track: codec=%s, channels=%d",
			audioFormat.Codec(), opusFormat.ChannelCount)

		// Create audio outgoing track with proper codec capabilities
		var caps webrtc.RTPCodecCapability

		switch opusFormat.ChannelCount {
		case 1, 2:
			caps = webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeOpus,
				ClockRate: 48000,
				Channels:  2,
				SDPFmtpLine: func() string {
					s := "minptime=10;useinbandfec=1"
					if opusFormat.ChannelCount == 2 {
						s += ";stereo=1;sprop-stereo=1"
					}
					return s
				}(),
			}
		default:
			ts.logger.Log(logger.Warn, "[Track Switcher] Unsupported Opus channel count: %d", opusFormat.ChannelCount)
			return -1
		}

		audioOutgoingTrack := &OutgoingTrack{
			Caps: caps,
		}
		pc.OutgoingTracks = append(pc.OutgoingTracks, audioOutgoingTrack)
		ts.logger.Log(logger.Info, "[Track Switcher] Created audio outgoing track: Opus @ 48kHz, %d channels", caps.Channels)

		// Create quality track
		track := &QualityTrack{
			ID:            audioTrackID,
			Type:          "audio",
			Label:         "Audio",
			Media:         media,
			Format:        audioFormat,
			firstPacket:   true,
			outgoingTrack: audioOutgoingTrack,
			switcher:      ts,
			Codec:         audioFormat.Codec(),
			Bitrate:       128000, // 128 kbps default
			Width:         0,
			Height:        0,
		}

		// Register audio data callback
		curTimestamp := uint32(0)

		r.OnData(media, opusFormat, func(u *unit.Unit) error {
			// Process each RTP packet from the unit
			for _, orig := range u.RTPPackets {
				pkt := &rtp.Packet{
					Header:  orig.Header,
					Payload: orig.Payload,
				}

				// Recompute timestamp from scratch
				// Chrome requires a precise timestamp that FFmpeg doesn't provide
				pkt.Timestamp = curTimestamp
				curTimestamp += uint32(opus.PacketDuration2(pkt.Payload))

				// Send packet with NTP timestamp
				ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp-u.RTPPackets[0].Timestamp), 48000))
				audioOutgoingTrack.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
			}

			return nil
		})

		ts.logger.Log(logger.Info, "[Track Switcher] Registered audio data callback")

		ts.tracks = append(ts.tracks, track)
		ts.logger.Log(logger.Info, "[Track Switcher] Setup audio track %d: %s (codec=%s, %d bps)",
			audioTrackID, track.Label, track.Codec, track.Bitrate)

		return audioTrackID
	}

	return -1 // No audio track found
}

// setupEncoder creates and initializes the appropriate encoder for a track
func (ts *TrackSwitcher) setupEncoder(track *QualityTrack) error {
	switch track.Codec {
	case "H264":
		encoder := &rtph264.Encoder{
			PayloadType:    96,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		if err := encoder.Init(); err != nil {
			return err
		}
		track.encoder = encoder

	case "H265":
		encoder := &rtph265.Encoder{
			PayloadType:    96,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		if err := encoder.Init(); err != nil {
			return err
		}
		track.encoder = encoder

	case "VP8":
		encoder := &rtpvp8.Encoder{
			PayloadType:    96,
			PayloadMaxSize: webrtcPayloadMaxSize,
		}
		if err := encoder.Init(); err != nil {
			return err
		}
		track.encoder = encoder

	case "VP9":
		encoder := &rtpvp9.Encoder{
			PayloadType:      96,
			PayloadMaxSize:   webrtcPayloadMaxSize,
			InitialPictureID: ptrOf(uint16(8445)),
		}
		if err := encoder.Init(); err != nil {
			return err
		}
		track.encoder = encoder

	default:
		return fmt.Errorf("unsupported codec: %s", track.Codec)
	}

	return nil
}

// registerTrackCallback registers the data callback for a quality track
func (ts *TrackSwitcher) registerTrackCallback(track *QualityTrack, r *stream.Reader) error {
	r.OnData(
		track.Media,
		track.Format,
		func(u *unit.Unit) error {
			// Skip if this track is not active
			if !track.active.Load() {
				return nil
			}

			if u.NilPayload() {
				return nil
			}

			// Handle first packet initialization
			if track.firstPacket {
				track.firstPacket = false
			} else {
				// Check for B-frames (H264 only)
				if track.Codec == "H264" && u.PTS < track.lastPTS {
					return fmt.Errorf("WebRTC doesn't support H264 streams with B-frames")
				}
			}
			track.lastPTS = u.PTS

			// ✅ Wait for keyframe after track switch to avoid glitches
			if track.waitingForKeyframe {
				if !isKeyframe(u, track.Codec) {
					// Drop non-keyframes until we get a keyframe
					return nil
				}
				// Keyframe received, resume normal playback
				track.waitingForKeyframe = false
				track.switcher.logger.Log(logger.Info,
					"[Track Switcher] Keyframe received on track %d (%s), resuming playback",
					track.ID, track.Label)
			}

			// Encode packets based on codec
			var packets []*rtp.Packet
			var err error

			switch encoder := track.encoder.(type) {
			case *rtph264.Encoder:
				packets, err = encoder.Encode(u.Payload.(unit.PayloadH264))
			case *rtph265.Encoder:
				packets, err = encoder.Encode(u.Payload.(unit.PayloadH265))
			case *rtpvp8.Encoder:
				packets, err = encoder.Encode(u.Payload.(unit.PayloadVP8))
			case *rtpvp9.Encoder:
				packets, err = encoder.Encode(u.Payload.(unit.PayloadVP9))
			default:
				return fmt.Errorf("unknown encoder type")
			}

			if err != nil {
				return nil // Encoding errors are not fatal
			}

			// Write packets with continuity handling
			for _, pkt := range packets {
				ntp := u.NTP.Add(timestampToDuration(int64(pkt.Timestamp), 90000))
				pkt.Timestamp += u.RTPPackets[0].Timestamp

				// Apply continuity corrections
				track.switcher.applyRTPContinuity(pkt)

				// Write to outgoing track
				if err := track.outgoingTrack.WriteRTPWithNTP(pkt, ntp); err != nil {
					// Don't log every write error, just skip this packet
					continue
				}
			}

			return nil
		},
	)

	return nil
}

// applyRTPContinuity ensures sequence number and timestamp continuity across track switches
func (ts *TrackSwitcher) applyRTPContinuity(pkt *rtp.Packet) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Handle first packet after track switch
	if ts.expectingSwitch {
		ts.expectingSwitch = false
		// Calculate offset to make this packet's seq/ts equal to expected values
		ts.seqNumOffset = int32(ts.expectedNextSeq) - int32(pkt.SequenceNumber)
		ts.timestampOffset = int64(ts.expectedNextTs) - int64(pkt.Timestamp)

		ts.logger.Log(logger.Debug, "[Track Switcher] First packet after switch: native_seq=%d, expected=%d, offset=%d",
			pkt.SequenceNumber, ts.expectedNextSeq, ts.seqNumOffset)
	}

	// Initialize on first packet ever
	if !ts.continuityInit {
		ts.seqNumOffset = 0
		ts.timestampOffset = 0
		ts.lastSeqNum = pkt.SequenceNumber
		ts.lastTimestamp = pkt.Timestamp
		ts.continuityInit = true
		return
	}

	// Apply offsets to maintain continuity
	newSeq := uint16(int32(pkt.SequenceNumber) + ts.seqNumOffset)
	newTs := uint32(int64(pkt.Timestamp) + ts.timestampOffset)

	pkt.SequenceNumber = newSeq
	pkt.Timestamp = newTs

	// Remember this packet for next continuity calculation
	ts.lastSeqNum = newSeq
	ts.lastTimestamp = newTs
}

// SwitchToTrack switches to a different quality track or audio-only mode
// Enhanced to support switching to audio track (audio-only mode)
func (ts *TrackSwitcher) SwitchToTrack(targetIdx int) error {
	if !ts.enabled.Load() {
		return fmt.Errorf("track switcher is disabled")
	}

	// Check if already switching
	if ts.switchInProgress.Load() {
		return fmt.Errorf("track switch already in progress")
	}

	ts.mu.Lock()
	if targetIdx < 0 || targetIdx >= len(ts.tracks) {
		ts.mu.Unlock()
		return fmt.Errorf("invalid track index: %d (available: 0-%d)", targetIdx, len(ts.tracks)-1)
	}

	targetTrack := ts.tracks[targetIdx]

	// ✅ NEW: Check if switching to audio-only mode
	switchingToAudio := (targetTrack.Type == "audio")

	if targetIdx == ts.currentTrackIdx {
		ts.mu.Unlock()
		ts.logger.Log(logger.Debug, "[Track Switcher] Already on track %d, no switch needed", targetIdx)
		return nil
	}

	oldIdx := ts.currentTrackIdx
	oldTrack := ts.tracks[oldIdx]
	newTrack := ts.tracks[targetIdx]

	// Capture current sequence number and timestamp for continuity
	expectedNextSeq := ts.lastSeqNum + 1
	expectedNextTs := ts.lastTimestamp

	ts.mu.Unlock()

	// Mark switch in progress
	ts.switchInProgress.Store(true)
	defer ts.switchInProgress.Store(false)

	if switchingToAudio {
		ts.logger.Log(logger.Info, "[Track Switcher] Switching to AUDIO-ONLY mode (track %d: %s)",
			targetIdx, newTrack.Label)
	} else {
		ts.logger.Log(logger.Info, "[Track Switcher] Switching from track %d (%s) to track %d (%s)",
			oldIdx, oldTrack.Label, targetIdx, newTrack.Label)
	}

	// Step 1: Deactivate old track (only if it's a video track)
	if oldTrack.Type == "video" {
		oldTrack.active.Store(false)
		ts.logger.Log(logger.Debug, "[Track Switcher] Deactivated video track %d", oldIdx)
	}

	// Step 2: Brief pause to ensure old track stops sending packets
	time.Sleep(20 * time.Millisecond)

	// Step 3: Handle audio-only mode
	if switchingToAudio {
		// In audio-only mode, video track stays inactive
		// Audio continues to play through the peer connection's audio track
		ts.audioOnlyMode.Store(true)
		ts.logger.Log(logger.Info, "[Track Switcher] Entered audio-only mode - video disabled")
	} else {
		// Exiting audio-only mode or switching between video tracks
		ts.audioOnlyMode.Store(false)

		// Mark that we're expecting first packet after switch
		// Offset will be calculated when first packet arrives
		ts.mu.Lock()
		ts.expectedNextSeq = expectedNextSeq
		ts.expectedNextTs = expectedNextTs
		ts.expectingSwitch = true
		newTrack.firstPacket = true
		newTrack.waitingForKeyframe = true // ✅ Wait for keyframe to avoid glitches
		ts.mu.Unlock()

		// Activate new video track
		newTrack.active.Store(true)
		ts.logger.Log(logger.Debug, "[Track Switcher] Activated video track %d, waiting for keyframe", targetIdx)
	}

	// Step 4: Update current track index
	ts.mu.Lock()
	ts.currentTrackIdx = targetIdx
	ts.mu.Unlock()

	if switchingToAudio {
		ts.logger.Log(logger.Info, "[Track Switcher] Track switch completed: %d (%s) -> AUDIO-ONLY",
			oldIdx, oldTrack.Label)
	} else {
		ts.logger.Log(logger.Info, "[Track Switcher] Track switch completed: %d (%s) -> %d (%s)",
			oldIdx, oldTrack.Label, targetIdx, newTrack.Label)
	}

	return nil
}

// GetCurrentTrack returns the currently active track index
func (ts *TrackSwitcher) GetCurrentTrack() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.currentTrackIdx
}

// IsAudioOnlyMode returns true if currently in audio-only mode
func (ts *TrackSwitcher) IsAudioOnlyMode() bool {
	return ts.audioOnlyMode.Load()
}

// GetAudioTrackID returns the audio track ID, or -1 if no audio track
func (ts *TrackSwitcher) GetAudioTrackID() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.audioTrackIdx
}

// GetTrackInfo returns information about all available tracks (video + audio)
func (ts *TrackSwitcher) GetTrackInfo() []wsprotocol.TrackInfo2 {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	info := make([]wsprotocol.TrackInfo2, len(ts.tracks))
	for i, track := range ts.tracks {
		info[i] = wsprotocol.TrackInfo2{
			ID:      track.ID,
			Type:    track.Type,
			Codec:   track.Codec,
			Label:   track.Label,
			Bitrate: track.Bitrate,
			Width:   track.Width,
			Height:  track.Height,
		}
	}
	return info
}

// Close cleans up the track switcher
func (ts *TrackSwitcher) Close() {
	ts.enabled.Store(false)

	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Deactivate all tracks
	for _, track := range ts.tracks {
		track.active.Store(false)
	}

	ts.logger.Log(logger.Info, "[Track Switcher] Closed")
}
