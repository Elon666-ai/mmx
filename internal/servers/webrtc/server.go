// Package webrtc contains a WebRTC server.
package webrtc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/gortsplib/v5/pkg/readbuffer"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/protocols/websocket"
	"github.com/bluenviron/mediamtx/internal/restrictnetwork"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	webrtcTurnSecretExpiration = 24 * time.Hour
)

// ErrSessionNotFound is returned when a session is not found.
var ErrSessionNotFound = errors.New("session not found")

func interfaceIsEmpty(i any) bool {
	return reflect.ValueOf(i).Kind() != reflect.Ptr || reflect.ValueOf(i).IsNil()
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

var webrtcNilLogger = logging.NewDefaultLeveledLoggerForScope("", 0, &nilWriter{})

func randInt63() (int64, error) {
	var b [8]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return 0, err
	}

	return int64(uint64(b[0]&0b01111111)<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])), nil
}

// https://cs.opensource.google/go/go/+/refs/tags/go1.20.4:src/math/rand/rand.go;l=119
func randInt63n(n int64) (int64, error) {
	if n&(n-1) == 0 { // n is power of two, can mask
		r, err := randInt63()
		if err != nil {
			return 0, err
		}
		return r & (n - 1), nil
	}

	maxVal := int64((1 << 63) - 1 - (1<<63)%uint64(n))

	v, err := randInt63()
	if err != nil {
		return 0, err
	}

	for v > maxVal {
		v, err = randInt63()
		if err != nil {
			return 0, err
		}
	}

	return v % n, nil
}

func randomTurnUser() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz1234567890"
	b := make([]byte, 20)
	for i := range b {
		j, err := randInt63n(int64(len(charset)))
		if err != nil {
			return "", err
		}

		b[i] = charset[int(j)]
	}

	return string(b), nil
}

type serverAPISessionsListRes struct {
	data *defs.APIWebRTCSessionList
	err  error
}

type serverAPISessionsListReq struct {
	res chan serverAPISessionsListRes
}

type serverAPISessionsGetRes struct {
	data *defs.APIWebRTCSession
	err  error
}

type serverAPISessionsGetReq struct {
	uuid uuid.UUID
	res  chan serverAPISessionsGetRes
}

type serverAPISessionsKickRes struct {
	err error
}

type serverAPISessionsKickReq struct {
	uuid uuid.UUID
	res  chan serverAPISessionsKickRes
}

type webRTCNewSessionRes struct {
	sx            *session
	answer        []byte
	errStatusCode int
	err           error
}

type webRTCNewSessionReq struct {
	pathName    string
	remoteAddr  string
	offer       []byte
	publish     bool
	httpRequest *http.Request
	res         chan webRTCNewSessionRes
}

type webRTCAddSessionCandidatesRes struct {
	sx  *session
	err error
}

type webRTCAddSessionCandidatesReq struct {
	pathName   string
	secret     uuid.UUID
	candidates []*pwebrtc.ICECandidateInit
	res        chan webRTCAddSessionCandidatesRes
}

type webRTCDeleteSessionRes struct {
	err error
}

type webRTCDeleteSessionReq struct {
	pathName string
	secret   uuid.UUID
	res      chan webRTCDeleteSessionRes
}

type webRTCRenegotiateSessionRes struct {
	sx     *session
	answer []byte
	err    error
}

type webRTCRenegotiateSessionReq struct {
	pathName string
	secret   uuid.UUID
	offer    []byte
	res      chan webRTCRenegotiateSessionRes
}

type serverMetrics interface {
	SetWebRTCServer(defs.APIWebRTCServer)
}

type serverPathManager interface {
	FindPathConf(req defs.PathFindPathConfReq) (*conf.Path, error)
	AddPublisher(req defs.PathAddPublisherReq) (defs.Path, *stream.Stream, error)
	AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error)
}

type serverParent interface {
	logger.Writer
}

// Server is a WebRTC server.
type Server struct {
	Address               string
	Encryption            bool
	ServerKey             string
	ServerCert            string
	AllowOrigins          []string
	TrustedProxies        conf.IPNetworks
	ReadTimeout           conf.Duration
	WriteTimeout          conf.Duration
	UDPReadBufferSize     uint
	LocalUDPAddress       string
	LocalTCPAddress       string
	IPsFromInterfaces     bool
	IPsFromInterfacesList []string
	AdditionalHosts       []string
	ICEServers            []conf.WebRTCICEServer
	HandshakeTimeout      conf.Duration
	TrackGatherTimeout    conf.Duration
	STUNGatherTimeout     conf.Duration
	ExternalCmdPool       *externalcmd.Pool
	Metrics               serverMetrics
	PathManager           serverPathManager
	Parent                serverParent

	// ABR WebSocket server
	ABREnable          bool
	ABRWSPath          string
	ABRMinBitrate      int
	ABRMaxBitrate      int
	ABRDefaultQuality  string
	ABRSwitchThreshold float64
	abrWSServer        *websocket.ABRServer

	ctx              context.Context
	ctxCancel        func()
	httpServer       *httpServer
	udpMuxLn         net.PacketConn
	tcpMuxLn         net.Listener
	iceUDPMux        ice.UDPMux
	iceTCPMux        *webrtc.TCPMuxWrapper
	sessions         map[*session]struct{}
	sessionsBySecret map[uuid.UUID]*session

	// in
	chNewSession           chan webRTCNewSessionReq
	chCloseSession         chan *session
	chAddSessionCandidates chan webRTCAddSessionCandidatesReq
	chDeleteSession        chan webRTCDeleteSessionReq
	chRenegotiateSession   chan webRTCRenegotiateSessionReq
	chAPISessionsList      chan serverAPISessionsListReq
	chAPISessionsGet       chan serverAPISessionsGetReq
	chAPIConnsKick         chan serverAPISessionsKickReq

	// out
	done chan struct{}
}

// Initialize initializes the server.
func (s *Server) Initialize() error {
	ctx, ctxCancel := context.WithCancel(context.Background())

	s.ctx = ctx
	s.ctxCancel = ctxCancel
	s.sessions = make(map[*session]struct{})
	s.sessionsBySecret = make(map[uuid.UUID]*session)
	s.chNewSession = make(chan webRTCNewSessionReq)
	s.chCloseSession = make(chan *session)
	s.chAddSessionCandidates = make(chan webRTCAddSessionCandidatesReq)
	s.chDeleteSession = make(chan webRTCDeleteSessionReq)
	s.chRenegotiateSession = make(chan webRTCRenegotiateSessionReq)
	s.chAPISessionsList = make(chan serverAPISessionsListReq)
	s.chAPISessionsGet = make(chan serverAPISessionsGetReq)
	s.chAPIConnsKick = make(chan serverAPISessionsKickReq)
	s.done = make(chan struct{})

	s.httpServer = &httpServer{
		address:            s.Address,
		encryption:         s.Encryption,
		serverKey:          s.ServerKey,
		serverCert:         s.ServerCert,
		allowOrigins:       s.AllowOrigins,
		trustedProxies:     s.TrustedProxies,
		readTimeout:        s.ReadTimeout,
		writeTimeout:       s.WriteTimeout,
		pathManager:        s.PathManager,
		parent:             s,
		abrEnable:          s.ABREnable,
		abrWSPath:          s.ABRWSPath,
		abrMinBitrate:      s.ABRMinBitrate,
		abrMaxBitrate:      s.ABRMaxBitrate,
		abrDefaultQuality:  s.ABRDefaultQuality,
		abrSwitchThreshold: s.ABRSwitchThreshold,
	}
	err := s.httpServer.initialize()
	if err != nil {
		ctxCancel()
		return err
	}

	if s.LocalUDPAddress != "" {
		s.udpMuxLn, err = net.ListenPacket(restrictnetwork.Restrict("udp", s.LocalUDPAddress))
		if err != nil {
			s.httpServer.close()
			ctxCancel()
			return err
		}

		if s.UDPReadBufferSize != 0 {
			err = readbuffer.SetReadBuffer(s.udpMuxLn.(*net.UDPConn), int(s.UDPReadBufferSize))
			if err != nil {
				s.udpMuxLn.Close()
				s.httpServer.close()
				ctxCancel()
				return err
			}
		}

		s.iceUDPMux = pwebrtc.NewICEUDPMux(webrtcNilLogger, s.udpMuxLn)
	}

	if s.LocalTCPAddress != "" {
		s.tcpMuxLn, err = net.Listen(restrictnetwork.Restrict("tcp", s.LocalTCPAddress))
		if err != nil {
			if s.udpMuxLn != nil {
				s.udpMuxLn.Close()
			}
			s.httpServer.close()
			ctxCancel()
			return err
		}

		s.iceTCPMux = &webrtc.TCPMuxWrapper{
			Mux: pwebrtc.NewICETCPMux(webrtcNilLogger, s.tcpMuxLn, 8),
			Ln:  s.tcpMuxLn,
		}
	}

	str := "listener opened on " + s.Address + " (HTTP)"
	if s.udpMuxLn != nil {
		str += ", " + s.LocalUDPAddress + " (ICE/UDP)"
	}
	if s.tcpMuxLn != nil {
		str += ", " + s.LocalTCPAddress + " (ICE/TCP)"
	}
	s.Log(logger.Info, str)

	go s.run()

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetWebRTCServer(s)
	}

	// Start ABR WebSocket server if enabled
	if s.ABREnable {
		if err := s.initializeABRWebSocket(); err != nil {
			return err
		}
	}

	return nil
}

// Log implements logger.Writer.
func (s *Server) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[WebRTC] "+format, args...)
}

// Close closes the server.
func (s *Server) Close() {
	s.Log(logger.Info, "listener is closing")

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetWebRTCServer(nil)
	}

	if s.abrWSServer != nil {
		s.abrWSServer.Close()
	}

	s.ctxCancel()
	<-s.done
}

func (s *Server) run() {
	defer close(s.done)

	var wg sync.WaitGroup

outer:
	for {
		select {
		case req := <-s.chNewSession:
			sx := &session{
				udpReadBufferSize:     s.UDPReadBufferSize,
				parentCtx:             s.ctx,
				ipsFromInterfaces:     s.IPsFromInterfaces,
				ipsFromInterfacesList: s.IPsFromInterfacesList,
				additionalHosts:       s.AdditionalHosts,
				iceUDPMux:             s.iceUDPMux,
				iceTCPMux:             s.iceTCPMux,
				handshakeTimeout:      s.HandshakeTimeout,
				trackGatherTimeout:    s.TrackGatherTimeout,
				stunGatherTimeout:     s.STUNGatherTimeout,
				req:                   req,
				wg:                    &wg,
				externalCmdPool:       s.ExternalCmdPool,
				pathManager:           s.PathManager,
				parent:                s,
			}
			sx.initialize()
			s.sessions[sx] = struct{}{}
			s.sessionsBySecret[sx.secret] = sx
			req.res <- webRTCNewSessionRes{sx: sx}

		case sx := <-s.chCloseSession:
			delete(s.sessions, sx)
			delete(s.sessionsBySecret, sx.secret)

		case req := <-s.chAddSessionCandidates:
			sx, ok := s.sessionsBySecret[req.secret]
			if !ok || sx.req.pathName != req.pathName {
				req.res <- webRTCAddSessionCandidatesRes{err: ErrSessionNotFound}
				continue
			}

			req.res <- webRTCAddSessionCandidatesRes{sx: sx}

		case req := <-s.chDeleteSession:
			sx, ok := s.sessionsBySecret[req.secret]
			if !ok || sx.req.pathName != req.pathName {
				req.res <- webRTCDeleteSessionRes{err: ErrSessionNotFound}
				continue
			}

			delete(s.sessions, sx)
			delete(s.sessionsBySecret, sx.secret)
			sx.Close()

			req.res <- webRTCDeleteSessionRes{}

		case req := <-s.chRenegotiateSession:
			s.Log(logger.Info, "=== LOOKUP RENEGOTIATE SESSION ===")
			s.Log(logger.Info, "Requested secret: %s", req.secret.String())
			s.Log(logger.Info, "Requested path: %s", req.pathName)

			sx, ok := s.sessionsBySecret[req.secret]
			if !ok {
				s.Log(logger.Error, "Session with secret %s not found", req.secret.String())
				s.Log(logger.Info, "Available sessions:")
				for secret, session := range s.sessionsBySecret {
					s.Log(logger.Info, "  Secret: %s, Path: %s, UUID: %s", secret.String(), session.req.pathName, session.uuid.String())
				}
				req.res <- webRTCRenegotiateSessionRes{err: ErrSessionNotFound}
				continue
			}

			if sx.req.pathName != req.pathName {
				s.Log(logger.Error, "Session found but path mismatch: expected %s, got %s", req.pathName, sx.req.pathName)
				req.res <- webRTCRenegotiateSessionRes{err: ErrSessionNotFound}
				continue
			}

			s.Log(logger.Info, "Session found successfully: UUID=%s", sx.uuid.String())
			req.res <- webRTCRenegotiateSessionRes{sx: sx}

		case req := <-s.chAPISessionsList:
			data := &defs.APIWebRTCSessionList{
				Items: []*defs.APIWebRTCSession{},
			}

			for sx := range s.sessions {
				data.Items = append(data.Items, sx.apiItem())
			}

			sort.Slice(data.Items, func(i, j int) bool {
				return data.Items[i].Created.Before(data.Items[j].Created)
			})

			req.res <- serverAPISessionsListRes{data: data}

		case req := <-s.chAPISessionsGet:
			sx := s.findSessionByUUID(req.uuid)
			if sx == nil {
				req.res <- serverAPISessionsGetRes{err: ErrSessionNotFound}
				continue
			}

			req.res <- serverAPISessionsGetRes{data: sx.apiItem()}

		case req := <-s.chAPIConnsKick:
			sx := s.findSessionByUUID(req.uuid)
			if sx == nil {
				req.res <- serverAPISessionsKickRes{err: ErrSessionNotFound}
				continue
			}

			delete(s.sessions, sx)
			delete(s.sessionsBySecret, sx.secret)
			sx.Close()

			req.res <- serverAPISessionsKickRes{}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	wg.Wait()

	s.httpServer.close()

	if s.udpMuxLn != nil {
		s.udpMuxLn.Close()
	}

	if s.tcpMuxLn != nil {
		s.tcpMuxLn.Close()
	}
}

func (s *Server) findSessionByUUID(uuid uuid.UUID) *session {
	for sx := range s.sessions {
		if sx.uuid == uuid {
			return sx
		}
	}
	return nil
}

func (s *Server) generateICEServers(clientConfig bool) ([]pwebrtc.ICEServer, error) {
	ret := make([]pwebrtc.ICEServer, 0, len(s.ICEServers))

	for _, server := range s.ICEServers {
		if !server.ClientOnly || clientConfig {
			if server.Username == "AUTH_SECRET" {
				expireDate := time.Now().Add(webrtcTurnSecretExpiration).Unix()

				user, err := randomTurnUser()
				if err != nil {
					return nil, err
				}

				server.Username = strconv.FormatInt(expireDate, 10) + ":" + user

				h := hmac.New(sha1.New, []byte(server.Password))
				h.Write([]byte(server.Username))

				server.Password = base64.StdEncoding.EncodeToString(h.Sum(nil))
			}

			ret = append(ret, pwebrtc.ICEServer{
				URLs:       []string{server.URL},
				Username:   server.Username,
				Credential: server.Password,
			})
		}
	}

	return ret, nil
}

// newSession is called by webRTCHTTPServer.
func (s *Server) newSession(req webRTCNewSessionReq) webRTCNewSessionRes {
	req.res = make(chan webRTCNewSessionRes)

	select {
	case s.chNewSession <- req:
		res := <-req.res

		return res.sx.new(req)

	case <-s.ctx.Done():
		return webRTCNewSessionRes{
			errStatusCode: http.StatusInternalServerError,
			err:           fmt.Errorf("terminated"),
		}
	}
}

// closeSession is called by session.
func (s *Server) closeSession(sx *session) {
	select {
	case s.chCloseSession <- sx:
	case <-s.ctx.Done():
	}
}

// addSessionCandidates is called by webRTCHTTPServer.
func (s *Server) addSessionCandidates(
	req webRTCAddSessionCandidatesReq,
) webRTCAddSessionCandidatesRes {
	req.res = make(chan webRTCAddSessionCandidatesRes)
	select {
	case s.chAddSessionCandidates <- req:
		res1 := <-req.res
		if res1.err != nil {
			return res1
		}

		return res1.sx.addCandidates(req)

	case <-s.ctx.Done():
		return webRTCAddSessionCandidatesRes{err: fmt.Errorf("terminated")}
	}
}

// deleteSession is called by webRTCHTTPServer.
func (s *Server) deleteSession(req webRTCDeleteSessionReq) error {
	req.res = make(chan webRTCDeleteSessionRes)
	select {
	case s.chDeleteSession <- req:
		res := <-req.res
		return res.err

	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}
}

// renegotiateSession is called by webRTCHTTPServer for SDP renegotiation.
func (s *Server) renegotiateSession(req webRTCRenegotiateSessionReq) webRTCRenegotiateSessionRes {
	s.Log(logger.Info, "=== RENEGOTIATE SESSION REQUEST ===")
	s.Log(logger.Info, "Looking for session with secret: %s", req.secret.String())

	req.res = make(chan webRTCRenegotiateSessionRes)
	select {
	case s.chRenegotiateSession <- req:
		res1 := <-req.res
		if res1.err != nil {
			s.Log(logger.Error, "Session lookup failed: %v", res1.err)
			return res1
		}

		s.Log(logger.Info, "Found session with UUID: %s", res1.sx.uuid.String())
		return res1.sx.renegotiate(req)

	case <-s.ctx.Done():
		return webRTCRenegotiateSessionRes{err: fmt.Errorf("terminated")}
	}
}

// APISessionsList is called by api.
func (s *Server) APISessionsList() (*defs.APIWebRTCSessionList, error) {
	req := serverAPISessionsListReq{
		res: make(chan serverAPISessionsListRes),
	}

	select {
	case s.chAPISessionsList <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APISessionsGet is called by api.
func (s *Server) APISessionsGet(uuid uuid.UUID) (*defs.APIWebRTCSession, error) {
	req := serverAPISessionsGetReq{
		uuid: uuid,
		res:  make(chan serverAPISessionsGetRes),
	}

	select {
	case s.chAPISessionsGet <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APISessionsKick is called by api.
func (s *Server) APISessionsKick(uuid uuid.UUID) error {
	req := serverAPISessionsKickReq{
		uuid: uuid,
		res:  make(chan serverAPISessionsKickRes),
	}

	select {
	case s.chAPIConnsKick <- req:
		res := <-req.res
		return res.err

	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}
}

// findSessionByID finds a session by its ID (secret UUID string).
func (s *Server) findSessionByID(sessionID string) *session {
	// Parse UUID from string
	secret, err := uuid.Parse(sessionID)
	if err != nil {
		s.Log(logger.Warn, "invalid session ID format: %s", sessionID)
		return nil
	}

	s.Log(logger.Debug, "Looking up session by secret UUID: %s", sessionID)

	// Try to find in the map
	// The sessionsBySecret map is populated when session is created
	// and removed when session is deleted

	// Since we don't have direct access with thread safety here,
	// we'll iterate through sessions
	sessionCount := 0
	for sess := range s.sessions {
		sessionCount++
		if sess.secret == secret {
			s.Log(logger.Debug, "Found session %s for secret %s", sess.uuid, sessionID)
			return sess
		}
	}

	s.Log(logger.Warn, "Session not found: %s (searched through %d active sessions)", sessionID, sessionCount)
	return nil
}

func (s *Server) initializeABRWebSocket() error {
	abrServer, err := websocket.NewABRServer(s.ABRWSPath, s, s)
	if err != nil {
		return err
	}

	s.abrWSServer = abrServer

	if err := s.abrWSServer.Start(); err != nil {
		return err
	}

	s.Log(logger.Info, "[ABR] WebSocket server started: %s", s.ABRWSPath)

	return nil
}

// OnABRMessage handles ABR control messages
func (s *Server) OnABRMessage(sessionID string, msg *websocket.ABRMessage) error {
	session := s.findSessionByID(sessionID)
	if session == nil {
		s.Log(logger.Error, "OnABRMessage failed: session not found: %s", sessionID)
		return fmt.Errorf("session not found: %s", sessionID)
	}

	switch msg.Type {
	case "SWITCH_QUALITY":
		// Parse quality as track ID (integer)
		trackID, ok := msg.Data.(float64)
		if !ok {
			s.Log(logger.Error, "Invalid SWITCH_QUALITY data: expected number, got %T", msg.Data)
			return fmt.Errorf("invalid track ID in SWITCH_QUALITY message")
		}

		s.Log(logger.Info, "Switching quality for session %s to track %d", sessionID, int(trackID))

		if err := session.SwitchVideoTrack(int(trackID)); err != nil {
			s.Log(logger.Error, "Failed to switch track for session %s: %v", sessionID, err)
			return fmt.Errorf("failed to switch track: %w", err)
		}

		s.Log(logger.Info, "Successfully switched quality for session %s to track %d", sessionID, int(trackID))
		return nil

	default:
		s.Log(logger.Warn, "Unknown ABR message type: %s", msg.Type)
		return fmt.Errorf("unknown message type: %s", msg.Type)
	}
}

// GetSessionTracks returns available tracks for a session
func (s *Server) GetSessionTracks(sessionID string) ([]websocket.TrackInfo2, error) {
	session := s.findSessionByID(sessionID)
	if session == nil {
		s.Log(logger.Error, "GetSessionTracks failed: session not found: %s", sessionID)
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	// Get tracks info from session
	tracksInfo := session.GetTracksInfo()
	return tracksInfo.Tracks, nil
}

// GetPathTracks gets track information from a path by finding existing sessions
// This avoids needing PathManager.Describe which doesn't exist in serverPathManager
func (s *Server) GetPathTracks(pathName string) ([]websocket.TrackInfo2, error) {
	s.Log(logger.Info, "[ABR] Getting tracks for path: %s", pathName)

	// Find any active session reading from this path
	var targetSession *session
	sessionCount := 0

	for sess := range s.sessions {
		sessionCount++
		// Compare with req.pathName
		if sess.req.pathName == pathName {
			targetSession = sess
			s.Log(logger.Info, "[ABR] Found session %s reading from path %s", sess.uuid, pathName)
			break
		}
	}

	s.Log(logger.Debug, "[ABR] Searched through %d sessions", sessionCount)

	if targetSession == nil {
		s.Log(logger.Warn, "[ABR] No active session found for path: %s, using default tracks", pathName)
		// Return default simulcast tracks
		return s.getDefaultTracks(), nil
	}

	// Get tracks info from the session
	// This will trigger lazy loading if needed
	tracksInfo := targetSession.GetTracksInfo()

	if len(tracksInfo.Tracks) == 0 {
		s.Log(logger.Warn, "[ABR] Session found but no tracks available, using defaults")
		return s.getDefaultTracks(), nil
	}

	s.Log(logger.Info, "[ABR] Retrieved %d tracks from path %s via session %s",
		len(tracksInfo.Tracks), pathName, targetSession.uuid)
	return tracksInfo.Tracks, nil
}

// getDefaultTracks returns default simulcast + audio track configuration
// This is used when no active session is found or as a fallback
func (s *Server) getDefaultTracks() []websocket.TrackInfo2 {
	s.Log(logger.Debug, "[ABR] Using default track configuration (3 video + 1 audio)")
	return []websocket.TrackInfo2{
		{
			ID:      0,
			Type:    "video",
			Codec:   "h264",
			Label:   "High",
			Bitrate: 2000000, // 2 Mbps
			Width:   1920,
			Height:  1080,
		},
		{
			ID:      1,
			Type:    "video",
			Codec:   "h264",
			Label:   "Medium",
			Bitrate: 1000000, // 1 Mbps
			Width:   1280,
			Height:  720,
		},
		{
			ID:      2,
			Type:    "video",
			Codec:   "h264",
			Label:   "Low",
			Bitrate: 400000, // 400 kbps
			Width:   960,
			Height:  540,
		},
		{
			ID:      3,
			Type:    "audio",
			Codec:   "opus",
			Label:   "Audio",
			Bitrate: 128000, // 128 kbps
			Width:   0,
			Height:  0,
		},
	}
}

// Helper functions for quality labels and bitrates
func (s *Server) getQualityLabel(trackID int) string {
	labels := []string{"High", "Medium", "Low"}
	if trackID < len(labels) {
		return labels[trackID]
	}
	return fmt.Sprintf("Track%d", trackID)
}

func (s *Server) getBitrateForQuality(trackID int) int {
	bitrates := []int{2000000, 1000000, 400000} // 2 Mbps, 1 Mbps, 400 kbps
	if trackID < len(bitrates) {
		return bitrates[trackID]
	}
	return 1000000
}

func (s *Server) getWidthForQuality(trackID int) int {
	widths := []int{1920, 1280, 960}
	if trackID < len(widths) {
		return widths[trackID]
	}
	return 1280
}

func (s *Server) getHeightForQuality(trackID int) int {
	heights := []int{1080, 720, 540}
	if trackID < len(heights) {
		return heights[trackID]
	}
	return 720
}
