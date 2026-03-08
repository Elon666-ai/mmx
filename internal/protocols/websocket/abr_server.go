package websocket

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	gws "github.com/gorilla/websocket"
)

// ABRServer is a standalone WebSocket server for ABR control
type ABRServer struct {
	address   string
	path      string
	handler   ABRMessageHandler
	log       logger.Writer
	listener  net.Listener
	httpSrv   *http.Server
	conns     map[string]*ABRConnection
	connsMux  sync.RWMutex
	closeChan chan struct{}
}

// ABRMessageHandler handles ABR control messages
type ABRMessageHandler interface {
	OnABRMessage(sessionID string, msg *ABRMessage) error
	GetSessionTracks(sessionID string) ([]TrackInfo2, error)
	GetPathTracks(pathName string) ([]TrackInfo2, error) // ✅ 添加这一行
}

// ABRMessage represents an ABR control message
type ABRMessage struct {
	MsgID     string      `json:"msg_id,omitempty"`
	Type      string      `json:"type"`
	ReplyTo   string      `json:"reply_to,omitempty"`
	SessionID string      `json:"session_id,omitempty"`
	Quality   string      `json:"quality,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

// TrackInfo represents track information
type TrackInfo struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Quality string `json:"quality"`
	Bitrate int    `json:"bitrate"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
}

// ABRConnection represents a WebSocket connection for ABR control
type ABRConnection struct {
	conn      *gws.Conn
	sessionID string
	pathName  string // ✅ 添加这一行
	server    *ABRServer
	writeMux  sync.Mutex
	closeChan chan struct{}
	closeOnce sync.Once
}

var upgrader = gws.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// NewABRServer creates a new ABR WebSocket server
func NewABRServer(wsURL string, handler ABRMessageHandler, log logger.Writer) (*ABRServer, error) {
	// Parse WebSocket URL
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebSocket URL: %v", err)
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("invalid WebSocket scheme: %s (must be ws or wss)", u.Scheme)
	}

	return &ABRServer{
		address:   u.Host,
		path:      u.Path,
		handler:   handler,
		log:       log,
		conns:     make(map[string]*ABRConnection),
		closeChan: make(chan struct{}),
	}, nil
}

// Start starts the WebSocket server
func (s *ABRServer) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", s.address, err)
	}

	s.log.Log(logger.Info, "[ABR-WS] WebSocket server listening on %s%s", s.address, s.path)

	mux := http.NewServeMux()
	mux.HandleFunc(s.path, s.handleWebSocket)

	s.httpSrv = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		err := s.httpSrv.Serve(s.listener)
		if err != nil && err != http.ErrServerClosed {
			s.log.Log(logger.Error, "[ABR-WS] Server error: %v", err)
		}
	}()

	return nil
}

// Close closes the WebSocket server
func (s *ABRServer) Close() error {
	close(s.closeChan)

	// Close all connections
	s.connsMux.Lock()
	for _, conn := range s.conns {
		conn.Close()
	}
	s.connsMux.Unlock()

	if s.httpSrv != nil {
		return s.httpSrv.Close()
	}

	return nil
}

// handleWebSocket handles WebSocket upgrade requests
func (s *ABRServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Get parameters from query
	sessionID := r.URL.Query().Get("session_id")
	pathName := r.URL.Query().Get("path") // ✅ 添加

	// ✅ 修改：至少需要一个参数
	if sessionID == "" && pathName == "" {
		s.log.Log(logger.Error, "[ABR-WS] Missing session_id or path parameter")
		http.Error(w, "missing session_id or path parameter", http.StatusBadRequest)
		return
	}

	// ✅ 添加：记录请求信息
	if pathName != "" {
		s.log.Log(logger.Info, "[ABR-WS] WebSocket request for path: %s", pathName)
	} else {
		s.log.Log(logger.Info, "[ABR-WS] WebSocket request for session: %s", sessionID)
	}

	s.log.Log(logger.Info, "[ABR-WS] WebSocket upgrade request for session: %s", sessionID)

	// Upgrade connection
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Log(logger.Error, "[ABR-WS] Failed to upgrade connection: %v", err)
		return
	}

	// Create ABR connection
	abrConn := &ABRConnection{
		conn:      conn,
		sessionID: sessionID,
		pathName:  pathName, // ✅ 添加这一行
		server:    s,
		closeChan: make(chan struct{}),
	}

	// Register connection
	s.connsMux.Lock()
	s.conns[sessionID] = abrConn
	s.connsMux.Unlock()

	// ✅ 使用更通用的日志
	lookupKey := pathName
	if lookupKey == "" {
		lookupKey = sessionID
	}
	s.log.Log(logger.Info, "[ABR-WS] WebSocket connected for: %s", lookupKey)

	// Send initial TRACKS_INFO with retry mechanism
	if err := abrConn.sendTracksInfo(); err != nil {
		s.log.Log(logger.Warn, "[ABR-WS] Initial sendTracksInfo failed: %v, will retry in 500ms", err)

		// ✅ 添加延迟重试
		go func() {
			time.Sleep(500 * time.Millisecond)
			s.log.Log(logger.Info, "[ABR-WS] Retrying sendTracksInfo...")

			if err := abrConn.sendTracksInfo(); err != nil {
				s.log.Log(logger.Error, "[ABR-WS] Retry also failed: %v, sending defaults", err)
				// Send default tracks as last resort
				if err := abrConn.sendDefaultTracks(); err != nil {
					s.log.Log(logger.Error, "[ABR-WS] Failed to send default tracks: %v", err)
				}
			} else {
				s.log.Log(logger.Info, "[ABR-WS] Retry succeeded")
			}
		}()
	} else {
		s.log.Log(logger.Info, "[ABR-WS] Initial sendTracksInfo succeeded")
	}

	// Start message handler
	go abrConn.handleMessages()
}

// GetConnection returns the connection for a session
func (s *ABRServer) GetConnection(sessionID string) *ABRConnection {
	s.connsMux.RLock()
	defer s.connsMux.RUnlock()
	return s.conns[sessionID]
}

// handleMessages handles incoming WebSocket messages
func (c *ABRConnection) handleMessages() {
	defer func() {
		c.Close()
		c.server.connsMux.Lock()
		delete(c.server.conns, c.sessionID)
		c.server.connsMux.Unlock()
	}()

	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Send ping periodically
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				if err := c.writePing(); err != nil {
					return
				}
			case <-c.closeChan:
				return
			}
		}
	}()

	for {
		var msg ABRMessage
		err := c.conn.ReadJSON(&msg)
		if err != nil {
			if gws.IsUnexpectedCloseError(err, gws.CloseGoingAway, gws.CloseAbnormalClosure) {
				c.server.log.Log(logger.Error, "[ABR-WS] Read error: %v", err)
			}
			return
		}

		c.server.log.Log(logger.Debug, "[ABR-WS] Received message: type=%s, session=%s",
			msg.Type, c.sessionID)

		// Handle PING messages directly at connection level
		if msg.Type == "PING" {
			c.server.log.Log(logger.Debug, "[ABR-WS] Received PING, sending PONG reply_to=%s", msg.MsgID)
			pongMsg := ABRMessage{
				MsgID:   fmt.Sprintf("pong_%d", time.Now().UnixNano()),
				Type:    "PONG",
				ReplyTo: msg.MsgID,
			}
			if err := c.writeJSON(&pongMsg); err != nil {
				c.server.log.Log(logger.Error, "[ABR-WS] Failed to send PONG: %v", err)
			} else {
				c.server.log.Log(logger.Debug, "[ABR-WS] PONG sent successfully")
			}
			continue
		}

		// Handle SELECT_LAYER messages directly at connection level
		if msg.Type == "SELECT_LAYER" {
			c.server.log.Log(logger.Debug, "[ABR-WS] Received SELECT_LAYER message")

			// Parse the payload
			var payload SelectLayerPayload
			payloadData, ok := msg.Data.(map[string]interface{})
			if !ok {
				c.server.log.Log(logger.Error, "[ABR-WS] Invalid SELECT_LAYER payload: expected map, got %T", msg.Data)
				c.sendError("Invalid SELECT_LAYER payload")
				continue
			}

			// Extract target_track_id
			if targetTrackID, ok := payloadData["target_track_id"].(float64); ok {
				payload.TargetTrackID = int(targetTrackID)
			} else {
				c.server.log.Log(logger.Error, "[ABR-WS] Missing or invalid target_track_id in SELECT_LAYER payload")
				c.sendError("Missing or invalid target_track_id")
				continue
			}

			// Extract reason (optional)
			if reason, ok := payloadData["reason"].(string); ok {
				payload.Reason = reason
			}

			c.server.log.Log(logger.Info, "[ABR-WS] Layer switch requested: target_track=%d, reason='%s'",
				payload.TargetTrackID, payload.Reason)

			// Call handler to perform the switch
			switchMsg := ABRMessage{
				Type: "SWITCH_QUALITY",
				Data: float64(payload.TargetTrackID), // Convert to match existing handler format
			}

			if err := c.server.handler.OnABRMessage(c.sessionID, &switchMsg); err != nil {
				c.server.log.Log(logger.Error, "[ABR-WS] Layer switch failed: %v", err)
				c.sendError(fmt.Sprintf("Layer switch failed: %v", err))
				continue
			}

			// Send LAYER_SWITCHED response
			response := ABRMessage{
				MsgID:   fmt.Sprintf("switched_%d", time.Now().UnixNano()),
				Type:    "LAYER_SWITCHED",
				ReplyTo: msg.MsgID,
				Data: map[string]interface{}{
					"success":          true,
					"current_track_id": payload.TargetTrackID,
				},
			}

			if err := c.writeJSON(&response); err != nil {
				c.server.log.Log(logger.Error, "[ABR-WS] Failed to send LAYER_SWITCHED response: %v", err)
			} else {
				c.server.log.Log(logger.Info, "[ABR-WS] Layer switch succeeded: target_track=%d", payload.TargetTrackID)
			}
			continue
		}

		// Handle other messages through handler
		if err := c.server.handler.OnABRMessage(c.sessionID, &msg); err != nil {
			c.server.log.Log(logger.Error, "[ABR-WS] Handler error: %v", err)
			c.sendError(err.Error())
		}
	}
}

// sendTracksInfo sends track information to the client
func (c *ABRConnection) sendTracksInfo() error {
	var tracks []TrackInfo2
	var err error

	// ✅ 优先从 path 获取（不依赖 session）
	if c.pathName != "" {
		c.server.log.Log(logger.Info, "[ABR-WS] Getting tracks from path: %s", c.pathName)
		tracks, err = c.server.handler.GetPathTracks(c.pathName)
	} else if c.sessionID != "" {
		// 回退到 session（向后兼容）
		c.server.log.Log(logger.Info, "[ABR-WS] Getting tracks from session: %s", c.sessionID)
		tracks, err = c.server.handler.GetSessionTracks(c.sessionID)
	} else {
		return fmt.Errorf("no path or session ID available")
	}

	if err != nil {
		c.server.log.Log(logger.Error, "[ABR-WS] Failed to get tracks: %v", err)
		return err
	}

	c.server.log.Log(logger.Info, "[ABR-WS] Retrieved %d tracks for session %s", len(tracks), c.sessionID)

	// Convert TrackInfo2 to the expected format for ABRMessage
	trackData := make([]map[string]interface{}, len(tracks))
	for i, track := range tracks {
		trackData[i] = map[string]interface{}{
			"id":      track.ID,
			"type":    track.Type,
			"codec":   track.Codec,
			"label":   track.Label,
			"bitrate": track.Bitrate,
			"width":   track.Width,
			"height":  track.Height,
		}
		c.server.log.Log(logger.Debug, "[ABR-WS] Track[%d]: id=%d, type=%s, codec=%s, label=%s, bitrate=%d, resolution=%dx%d",
			i, track.ID, track.Type, track.Codec, track.Label, track.Bitrate, track.Width, track.Height)
	}

	msg := ABRMessage{
		MsgID: fmt.Sprintf("tracks_%d", time.Now().UnixNano()),
		Type:  "TRACKS_INFO",
		Data:  trackData,
	}

	c.server.log.Log(logger.Info, "[ABR-WS] Sending TRACKS_INFO with %d tracks", len(trackData))
	return c.writeJSON(&msg)
}

// SendQualityChanged sends quality change notification
func (c *ABRConnection) SendQualityChanged(quality string) error {
	msg := ABRMessage{
		MsgID:   fmt.Sprintf("quality_%d", time.Now().UnixNano()),
		Type:    "QUALITY_CHANGED",
		Quality: quality,
	}
	return c.writeJSON(&msg)
}

// writeJSON writes a JSON message to the WebSocket
func (c *ABRConnection) writeJSON(msg interface{}) error {
	c.writeMux.Lock()
	defer c.writeMux.Unlock()

	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

// writePing writes a ping message
func (c *ABRConnection) writePing() error {
	c.writeMux.Lock()
	defer c.writeMux.Unlock()

	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(gws.PingMessage, nil)
}

// sendError sends an error message to the client
func (c *ABRConnection) sendError(errMsg string) error {
	msg := ABRMessage{
		MsgID: fmt.Sprintf("error_%d", time.Now().UnixNano()),
		Type:  "ERROR",
		Data:  map[string]string{"error": errMsg},
	}
	return c.writeJSON(&msg)
}

// Close closes the WebSocket connection
func (c *ABRConnection) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeChan)
		err = c.conn.Close()
		c.server.log.Log(logger.Info, "[ABR-WS] Connection closed for session: %s", c.sessionID)
	})
	return err
}

// sendDefaultTracks sends a default track configuration when actual tracks cannot be retrieved
func (c *ABRConnection) sendDefaultTracks() error {
	c.server.log.Log(logger.Info, "[ABR-WS] Sending default tracks configuration")

	// Default simulcast setup: 3 video qualities + 1 audio
	tracks := []TrackInfo2{
		{ID: 0, Type: "video", Codec: "h264", Label: "High", Bitrate: 2000000, Width: 1920, Height: 1080},
		{ID: 1, Type: "video", Codec: "h264", Label: "Medium", Bitrate: 1000000, Width: 1280, Height: 720},
		{ID: 2, Type: "video", Codec: "h264", Label: "Low", Bitrate: 400000, Width: 960, Height: 540},
		{ID: 3, Type: "audio", Codec: "opus", Label: "Audio", Bitrate: 128000, Width: 48, Height: 27},
	}

	// Convert to message format
	trackData := make([]map[string]interface{}, len(tracks))
	for i, track := range tracks {
		trackData[i] = map[string]interface{}{
			"id":      track.ID,
			"type":    track.Type,
			"codec":   track.Codec,
			"label":   track.Label,
			"bitrate": track.Bitrate,
			"width":   track.Width,
			"height":  track.Height,
		}
		c.server.log.Log(logger.Debug, "[ABR-WS] Default Track[%d]: id=%d, type=%s, label=%s",
			i, track.ID, track.Type, track.Label)
	}

	msg := ABRMessage{
		MsgID: fmt.Sprintf("tracks_default_%d", time.Now().UnixNano()),
		Type:  "TRACKS_INFO",
		Data:  trackData,
	}

	c.server.log.Log(logger.Info, "[ABR-WS] Sending default TRACKS_INFO with %d tracks", len(tracks))
	return c.writeJSON(&msg)
}
