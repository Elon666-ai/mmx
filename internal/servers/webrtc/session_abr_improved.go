package webrtc

// ABR Protocol Implementation - Enhanced Version
// This file contains improved ABR (Adaptive Bitrate) functionality that:
// 1. Works with any number of tracks (including single track)
// 2. Supports both edge nodes (with trackSwitcher) and origin nodes (without trackSwitcher)
// 3. Extracts track info from peer connection when trackSwitcher is not available
// 4. Includes audio track information for audio-only mode support

import (
	"errors"

	"github.com/bluenviron/mediamtx/internal/logger"
	wsprotocol "github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

// CollectTrackInfoImproved collects track information with audio track support.
// This method works in two modes:
// 1. Edge node (with trackSwitcher): Gets video tracks from switcher + audio from peer connection
// 2. Origin node (without trackSwitcher): Gets all tracks from peer connection
func (s *session) CollectTrackInfoImproved() []wsprotocol.TrackInfo2 {
	tracks := []wsprotocol.TrackInfo2{}

	// Priority 1: Use track switcher if available (edge node with ABR)
	if s.trackSwitcher != nil {
		videoTracks := s.trackSwitcher.GetTrackInfo()
		tracks = append(tracks, videoTracks...)

		if len(tracks) > 0 {
			s.Log(logger.Info, "ABR: Collected %d video tracks from track switcher", len(tracks))

			// Add audio track with ID after all video tracks
			audioTrack := s.getAudioTrackInfo(len(tracks))
			if audioTrack != nil {
				tracks = append(tracks, *audioTrack)
				s.Log(logger.Info, "ABR: Added audio track with ID %d", audioTrack.ID)
			}

			return tracks
		}
	}

	// Priority 2: Extract from peer connection (origin node without trackSwitcher)
	s.Log(logger.Debug, "ABR: No track switcher, extracting tracks from peer connection")
	tracks = s.getTracksFromPeerConnection()

	if len(tracks) > 0 {
		s.Log(logger.Info, "ABR: Collected %d tracks from peer connection", len(tracks))
		return tracks
	}

	// Final fallback: create single track
	s.Log(logger.Warn, "ABR: No tracks collected, using single-track fallback")
	return s.createSingleTrackFallback()
}

// getTracksFromPeerConnection extracts track information directly from peer connection
// This is used when trackSwitcher is not available (origin node)
// NOTE: Caller must hold s.mutex lock
func (s *session) getTracksFromPeerConnection() []wsprotocol.TrackInfo2 {
	if s.pc == nil {
		s.Log(logger.Warn, "ABR: No peer connection available")
		return nil
	}

	tracks := []wsprotocol.TrackInfo2{}
	trackID := 0

	// Extract video tracks first
	for _, track := range s.pc.OutgoingTracks {
		mimeType := track.Caps.MimeType

		// Check for video MIME types
		if mimeType == "video/H264" ||
			mimeType == "video/H265" ||
			mimeType == "video/VP8" ||
			mimeType == "video/VP9" ||
			mimeType == "video/AV1" {

			codec := s.getVideoCodecFromMime(mimeType)

			// For origin node, we typically have one video track
			trackInfo := wsprotocol.TrackInfo2{
				ID:      trackID,
				Type:    "video",
				Codec:   codec,
				Label:   "Main",
				Bitrate: 2000000, // 2 Mbps default
				Width:   1920,
				Height:  1080,
			}

			tracks = append(tracks, trackInfo)
			s.Log(logger.Debug, "ABR: Found video track: id=%d, codec=%s, mime=%s", trackID, codec, mimeType)
			trackID++
		}
	}

	// Extract audio tracks (after video tracks)
	for _, track := range s.pc.OutgoingTracks {
		mimeType := track.Caps.MimeType

		// Check for audio MIME types
		if mimeType == "audio/opus" ||
			mimeType == "audio/pcmu" ||
			mimeType == "audio/pcma" ||
			mimeType == "audio/g722" {

			codec := s.getCodecFromMime(mimeType)

			trackInfo := wsprotocol.TrackInfo2{
				ID:      trackID,
				Type:    "audio",
				Codec:   codec,
				Label:   "Audio",
				Bitrate: 128000, // 128 kbps default
				Width:   0,
				Height:  0,
			}

			tracks = append(tracks, trackInfo)
			s.Log(logger.Debug, "ABR: Found audio track: id=%d, codec=%s, mime=%s", trackID, codec, mimeType)
			trackID++
		}
	}

	return tracks
}

// getAudioTrackInfo extracts audio track information from peer connection
// audioTrackID should be assigned after all video tracks (as the last track)
// This is used when we have trackSwitcher (edge node)
// NOTE: Caller must hold s.mutex lock
func (s *session) getAudioTrackInfo(audioTrackID int) *wsprotocol.TrackInfo2 {
	if s.pc == nil {
		s.Log(logger.Debug, "ABR: No peer connection available for audio track extraction")
		return nil
	}

	// Look for audio in outgoing tracks
	for _, track := range s.pc.OutgoingTracks {
		mimeType := track.Caps.MimeType

		// Check for audio MIME types
		if mimeType == "audio/opus" ||
			mimeType == "audio/pcmu" ||
			mimeType == "audio/pcma" ||
			mimeType == "audio/g722" {

			codec := s.getCodecFromMime(mimeType)
			s.Log(logger.Debug, "ABR: Found audio track: codec=%s, mime=%s", codec, mimeType)

			return &wsprotocol.TrackInfo2{
				ID:      audioTrackID,
				Type:    "audio",
				Codec:   codec,
				Label:   "Audio",
				Bitrate: 128000, // 128 kbps default
				Width:   0,
				Height:  0,
			}
		}
	}

	s.Log(logger.Debug, "ABR: No audio track found in peer connection outgoing tracks")
	return nil
}

// getVideoCodecFromMime extracts video codec name from MIME type
func (s *session) getVideoCodecFromMime(mimeType string) string {
	switch mimeType {
	case "video/H264":
		return "h264"
	case "video/H265":
		return "h265"
	case "video/VP8":
		return "vp8"
	case "video/VP9":
		return "vp9"
	case "video/AV1":
		return "av1"
	default:
		return "h264" // default to h264
	}
}

// getCodecFromMime extracts audio codec name from MIME type
func (s *session) getCodecFromMime(mimeType string) string {
	switch mimeType {
	case "audio/opus":
		return "opus"
	case "audio/pcmu":
		return "pcmu"
	case "audio/pcma":
		return "pcma"
	case "audio/g722":
		return "g722"
	default:
		return "opus" // default to opus
	}
}

// getCodecName extracts codec name from format
func (s *session) getCodecName(format interface{}) string {
	// Try to get codec name from format
	if codecer, ok := format.(interface{ Codec() string }); ok {
		codec := codecer.Codec()
		// Map common codecs to WebRTC names
		switch codec {
		case "H264":
			return "h264"
		case "H265":
			return "h265"
		case "VP8":
			return "vp8"
		case "VP9":
			return "vp9"
		case "AV1":
			return "av1"
		case "Opus":
			return "opus"
		case "G722":
			return "g722"
		default:
			return "h264" // Default for video
		}
	}
	return "h264"
}

// getQualityLabel returns quality label based on track index
func (s *session) getQualityLabel(trackID int) string {
	switch trackID {
	case 0:
		return "High"
	case 1:
		return "Medium"
	case 2:
		return "Low"
	default:
		return "Auto"
	}
}

// getBitrateForQuality returns bitrate based on track index
func (s *session) getBitrateForQuality(trackID int) int {
	switch trackID {
	case 0:
		return 2000000 // 2.5 Mbps
	case 1:
		return 1000000 // 1.2 Mbps
	case 2:
		return 400000 // 500 kbps
	default:
		return 1000000 // 1 Mbps
	}
}

// getWidthForQuality returns width based on track index
func (s *session) getWidthForQuality(trackID int) int {
	switch trackID {
	case 0:
		return 1920
	case 1:
		return 1280
	case 2:
		return 960
	default:
		return 1280
	}
}

// getHeightForQuality returns height based on track index
func (s *session) getHeightForQuality(trackID int) int {
	switch trackID {
	case 0:
		return 1080
	case 1:
		return 720
	case 2:
		return 540
	default:
		return 720
	}
}

// createSingleTrackFallback creates a fallback configuration with a single track
// This ensures ABR protocol remains functional even without simulcast
func (s *session) createSingleTrackFallback() []wsprotocol.TrackInfo2 {
	return []wsprotocol.TrackInfo2{
		{
			ID:      0,
			Type:    "video",
			Codec:   "h264",
			Label:   "Main",
			Bitrate: 2000000, // 2 Mbps default
			Width:   1920,
			Height:  1080,
		},
	}
}

// ValidateTrackSwitch validates if a track switch is allowed
func (s *session) ValidateTrackSwitch(trackID int) error {
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	// If we have no track switcher, single track mode is in effect
	if s.trackSwitcher == nil {
		if trackID != 0 {
			return errors.New("only track 0 is available (single-track mode)")
		}
		return nil
	}

	// With track switcher, validate against available tracks
	// Get track info to check if trackID is valid
	trackInfo := s.trackSwitcher.GetTrackInfo()
	for _, track := range trackInfo {
		if track.ID == trackID {
			return nil // Valid track ID found
		}
	}

	return errors.New("invalid track ID")
}
