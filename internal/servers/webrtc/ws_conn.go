package webrtc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

// WSControlConnection represents a WebSocket control connection.
type WSControlConnection struct {
	conn      *websocket.ServerConn
	session   *session
	ctx       context.Context
	ctxCancel func()
	closeOnce sync.Once
}

// NewWSControlConnection creates a new WebSocket control connection.
func NewWSControlConnection(conn *websocket.ServerConn, session *session) *WSControlConnection {
	ctx, cancel := context.WithCancel(context.Background())

	wsc := &WSControlConnection{
		conn:      conn,
		session:   session,
		ctx:       ctx,
		ctxCancel: cancel,
	}

	session.Log(logger.Info, "[WS] WebSocket control connection created for session %s from %s",
		session.uuid, conn.RemoteAddr())

	// Start read pump
	go wsc.readPump()

	return wsc
}

// readPump pumps messages from the websocket connection to the handler.
func (wsc *WSControlConnection) readPump() {
	wsc.session.Log(logger.Debug, "[WS] Read pump started")

	defer func() {
		wsc.session.Log(logger.Debug, "[WS] Read pump stopped, closing connection")
		wsc.Close()
	}()

	for {
		select {
		case <-wsc.ctx.Done():
			wsc.session.Log(logger.Debug, "[WS] Context cancelled, exiting read pump")
			return
		default:
			var msg websocket.WSMessage
			err := wsc.conn.ReadJSON(&msg)
			if err != nil {
				wsc.session.Log(logger.Debug, "[WS] Read error: %v", err)
				return
			}

			wsc.session.Log(logger.Debug, "[WS] ← Received message: type=%s, msg_id=%s",
				msg.Type, msg.MsgID)

			// Handle message
			if err := wsc.handleMessage(&msg); err != nil {
				wsc.session.Log(logger.Error, "[WS] Error handling message: %v", err)
			}
		}
	}
}

// handleMessage handles incoming WebSocket messages.
func (wsc *WSControlConnection) handleMessage(msg *websocket.WSMessage) error {
	wsc.session.Log(logger.Debug, "[WS] Handling message: type=%s, msg_id=%s, timestamp=%d",
		msg.Type, msg.MsgID, msg.Timestamp)

	switch msg.Type {
	case websocket.MsgTypePING:
		wsc.session.Log(logger.Debug, "[WS] Processing PING message")
		return wsc.handlePing(msg)

	case websocket.MsgTypeSELECTLAYER:
		wsc.session.Log(logger.Debug, "[WS] Processing SELECT_LAYER message")
		return wsc.handleSelectLayer(msg)

	default:
		wsc.session.Log(logger.Warn, "[WS] Unknown message type: %s", msg.Type)
		return wsc.sendError(msg.MsgID, 400, fmt.Sprintf("unknown message type: %s", msg.Type))
	}
}

// handlePing handles PING messages.
func (wsc *WSControlConnection) handlePing(msg *websocket.WSMessage) error {
	wsc.session.Log(logger.Debug, "[WS] Received PING, sending PONG reply_to=%s", msg.MsgID)

	pong := websocket.NewWSMessage(websocket.MsgTypePONG)
	pong.ReplyTo = msg.MsgID

	if err := wsc.SendMessage(pong); err != nil {
		wsc.session.Log(logger.Error, "[WS] Failed to send PONG: %v", err)
		return err
	}

	wsc.session.Log(logger.Debug, "[WS] PONG sent successfully")
	return nil
}

// handleSelectLayer handles SELECT_LAYER messages.
func (wsc *WSControlConnection) handleSelectLayer(msg *websocket.WSMessage) error {
	var payload websocket.SelectLayerPayload
	if err := msg.GetPayloadAs(&payload); err != nil {
		wsc.session.Log(logger.Error, "[WS] Invalid SELECT_LAYER payload: %v", err)
		return wsc.sendError(msg.MsgID, 400, "invalid payload")
	}

	wsc.session.Log(logger.Info, "[WS] Layer switch requested: target_track=%d, reason='%s'",
		payload.TargetTrackID, payload.Reason)

	// Measure switch time
	startTime := time.Now()

	// Call session's SwitchVideoTrack method
	if err := wsc.session.SwitchVideoTrack(payload.TargetTrackID); err != nil {
		wsc.session.Log(logger.Error, "[WS] Track switch failed: target_track=%d, error=%v",
			payload.TargetTrackID, err)
		return wsc.sendError(msg.MsgID, 500, fmt.Sprintf("failed to switch track: %v", err))
	}

	switchTime := time.Since(startTime).Milliseconds()
	wsc.session.Log(logger.Info, "[WS] Track switch succeeded: target_track=%d, duration=%dms",
		payload.TargetTrackID, switchTime)

	// Send LAYER_SWITCHED response
	response := websocket.NewWSMessage(websocket.MsgTypeLAYERSWITCHED)
	response.ReplyTo = msg.MsgID
	response.SetPayload(websocket.LayerSwitchedPayload{
		Success:        true,
		CurrentTrackID: payload.TargetTrackID,
		SwitchTimeMs:   int(switchTime),
	})

	if err := wsc.SendMessage(response); err != nil {
		wsc.session.Log(logger.Error, "[WS] Failed to send LAYER_SWITCHED response: %v", err)
		return err
	}

	wsc.session.Log(logger.Debug, "[WS] LAYER_SWITCHED response sent successfully")
	return nil
}

// sendError sends an ERROR message.
func (wsc *WSControlConnection) sendError(replyTo string, code int, message string) error {
	wsc.session.Log(logger.Warn, "[WS] Sending ERROR response: code=%d, message='%s', reply_to=%s",
		code, message, replyTo)

	errMsg := websocket.NewWSMessage(websocket.MsgTypeERROR)
	errMsg.ReplyTo = replyTo
	errMsg.SetPayload(websocket.ErrorPayload{
		Code:    code,
		Message: message,
	})

	if err := wsc.SendMessage(errMsg); err != nil {
		wsc.session.Log(logger.Error, "[WS] Failed to send ERROR message: %v", err)
		return err
	}

	wsc.session.Log(logger.Debug, "[WS] ERROR message sent successfully")
	return nil
}

// SendMessage sends a WebSocket message.
func (wsc *WSControlConnection) SendMessage(msg *websocket.WSMessage) error {
	wsc.session.Log(logger.Debug, "[WS] → Sending message: type=%s, msg_id=%s, reply_to=%s",
		msg.Type, msg.MsgID, msg.ReplyTo)

	if err := wsc.conn.WriteJSON(msg); err != nil {
		wsc.session.Log(logger.Error, "[WS] Write error: %v", err)
		return err
	}

	wsc.session.Log(logger.Debug, "[WS] Message sent successfully: type=%s", msg.Type)
	return nil
}

// SendTracksInfo sends TRACKS_INFO message to client.
func (wsc *WSControlConnection) SendTracksInfo() error {
	tracksInfo := wsc.session.GetTracksInfo()

	wsc.session.Log(logger.Info, "[WS] Preparing TRACKS_INFO: %d tracks available",
		len(tracksInfo.Tracks))

	// Log each track info for debugging
	for i, track := range tracksInfo.Tracks {
		wsc.session.Log(logger.Debug, "[WS]   Track[%d]: id=%d, type=%s, codec=%s, label=%s, bitrate=%d, resolution=%dx%d",
			i, track.ID, track.Type, track.Codec, track.Label, track.Bitrate, track.Width, track.Height)
	}

	msg := websocket.NewWSMessage(websocket.MsgTypeTRACKSINFO)
	if err := msg.SetPayload(tracksInfo); err != nil {
		wsc.session.Log(logger.Error, "[WS] Failed to set TRACKS_INFO payload: %v", err)
		return fmt.Errorf("failed to set payload: %w", err)
	}

	wsc.session.Log(logger.Info, "[WS] Sending TRACKS_INFO: active_track=%d, total_tracks=%d",
		tracksInfo.ActiveTrackID, len(tracksInfo.Tracks))

	if err := wsc.SendMessage(msg); err != nil {
		wsc.session.Log(logger.Error, "[WS] Failed to send TRACKS_INFO: %v", err)
		return err
	}

	wsc.session.Log(logger.Debug, "[WS] TRACKS_INFO sent successfully")
	return nil
}

// Close closes the WebSocket connection.
func (wsc *WSControlConnection) Close() {
	wsc.closeOnce.Do(func() {
		wsc.session.Log(logger.Info, "[WS] Closing WebSocket control connection")

		wsc.ctxCancel()
		wsc.conn.Close()

		// Notify session
		if wsc.session != nil {
			wsc.session.Log(logger.Debug, "[WS] Notifying session of WebSocket closure")
			wsc.session.onWSClosed()
		}

		wsc.session.Log(logger.Info, "[WS] WebSocket control connection closed")
	})
}
