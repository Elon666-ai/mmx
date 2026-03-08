// Package websocket contains WebSocket control protocol for Simulcast ABR.
package websocket

import (
	"encoding/json"
	"time"
)

// Message types
const (
	MsgTypeTRACKSINFO    = "TRACKS_INFO"
	MsgTypeSELECTLAYER   = "SELECT_LAYER"
	MsgTypeLAYERSWITCHED = "LAYER_SWITCHED"
	MsgTypeERROR         = "ERROR"
	MsgTypePING          = "PING"
	MsgTypePONG          = "PONG"
)

// WSMessage is the base message structure for all WebSocket messages.
type WSMessage struct {
	MsgID     string                 `json:"msg_id"`
	Type      string                 `json:"type"`
	Timestamp int64                  `json:"timestamp"`
	Payload   map[string]interface{} `json:"payload,omitempty"`
	ReplyTo   string                 `json:"reply_to,omitempty"`
}

// TrackInfo contains metadata about a single track.
type TrackInfo2 struct {
	ID      int    `json:"id"`
	Type    string `json:"type"`             // "video" or "audio"
	Codec   string `json:"codec"`            // "h264", "opus", etc.
	Label   string `json:"label"`            // "High", "Medium", "Low", "audio"
	Bitrate int    `json:"bitrate"`          // bps
	Width   int    `json:"width,omitempty"`  // video only
	Height  int    `json:"height,omitempty"` // video only
}

// TracksInfoPayload is the payload for TRACKS_INFO message.
type TracksInfoPayload struct {
	ActiveTrackID int         `json:"active_track_id"`
	Tracks        []TrackInfo2 `json:"tracks"`
}

// SelectLayerPayload is the payload for SELECT_LAYER message.
type SelectLayerPayload struct {
	TargetTrackID int    `json:"target_track_id"`
	Reason        string `json:"reason,omitempty"` // optional: reason for switching
}

// LayerSwitchedPayload is the payload for LAYER_SWITCHED message.
type LayerSwitchedPayload struct {
	Success        bool `json:"success"`
	CurrentTrackID int  `json:"current_track_id"`
	SwitchTimeMs   int  `json:"switch_time_ms"` // switch duration in milliseconds
}

// ErrorPayload is the payload for ERROR message.
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NewWSMessage creates a new WebSocket message with timestamp.
func NewWSMessage(msgType string) *WSMessage {
	return &WSMessage{
		Type:      msgType,
		Timestamp: time.Now().Unix(),
		Payload:   make(map[string]interface{}),
	}
}

// SetPayload sets the payload for a message.
func (m *WSMessage) SetPayload(payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var p map[string]interface{}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}

	m.Payload = p
	return nil
}

// GetPayloadAs unmarshals the payload into the given type.
func (m *WSMessage) GetPayloadAs(v interface{}) error {
	data, err := json.Marshal(m.Payload)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, v)
}

// ToJSON converts the message to JSON bytes.
func (m *WSMessage) ToJSON() ([]byte, error) {
	return json.Marshal(m)
}

// FromJSON parses JSON bytes into a WSMessage.
func FromJSON(data []byte) (*WSMessage, error) {
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// GenerateLabelVertical generates label for vertical (portrait) streams.
// Vertical stream: height > width, judge by width.
func GenerateLabelVertical(trackID, width, height int) string {
	if width >= 1080 {
		return "High"
	} else if width >= 720 {
		return "Medium"
	} else if width >= 540 {
		return "Low"
	}
	return "audio"
}

// GenerateLabelHorizon generates label for horizontal (landscape) streams.
// Horizontal stream: width >= height, judge by height.
func GenerateLabelHorizon(trackID, width, height int) string {
	if height >= 1080 {
		return "High"
	} else if height >= 720 {
		return "Medium"
	} else if height >= 540 {
		return "Low"
	}
	return "audio"
}

// GenerateLabel generates appropriate label based on stream orientation.
func GenerateLabel(trackID, width, height int) string {
	if height > width {
		// Vertical (portrait) stream
		return GenerateLabelVertical(trackID, width, height)
	}
	// Horizontal (landscape) stream
	return GenerateLabelHorizon(trackID, width, height)
}
