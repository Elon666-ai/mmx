package webrtc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	"github.com/pion/sdp/v3"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	wsprotocol "github.com/bluenviron/mediamtx/internal/protocols/websocket"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/worker/models"
)

func whipOffer(body []byte) *pwebrtc.SessionDescription {
	return &pwebrtc.SessionDescription{
		Type: pwebrtc.SDPTypeOffer,
		SDP:  string(body),
	}
}

type sessionParent interface {
	closeSession(sx *session)
	generateICEServers(clientConfig bool) ([]pwebrtc.ICEServer, error)
	logger.Writer
}

type session struct {
	udpReadBufferSize     uint
	parentCtx             context.Context
	ipsFromInterfaces     bool
	ipsFromInterfacesList []string
	additionalHosts       []string
	iceUDPMux             ice.UDPMux
	iceTCPMux             *webrtc.TCPMuxWrapper
	handshakeTimeout      conf.Duration
	trackGatherTimeout    conf.Duration
	stunGatherTimeout     conf.Duration
	req                   webRTCNewSessionReq
	wg                    *sync.WaitGroup
	externalCmdPool       *externalcmd.Pool
	pathManager           serverPathManager
	parent                sessionParent

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	secret    uuid.UUID
	mutex     sync.RWMutex
	pc        *webrtc.PeerConnection

	chNew           chan webRTCNewSessionReq
	chAddCandidates chan webRTCAddSessionCandidatesReq
	chRenegotiate   chan webRTCRenegotiateSessionReq

	// ABR state
	currentBandwidthLimit int // Current bandwidth limit in kbps (0 = unlimited)

	// ABR Track Switching
	trackSwitcher *webrtc.TrackSwitcher

	// ABR WebSocket Control
	wsConn          *WSControlConnection
	currentTrackID  int
	availableTracks []wsprotocol.TrackInfo2
	trackMutex      sync.RWMutex
}

func (s *session) initialize() {
	ctx, ctxCancel := context.WithCancel(s.parentCtx)

	s.ctx = ctx
	s.ctxCancel = ctxCancel
	s.created = time.Now()
	s.uuid = uuid.New()
	s.secret = uuid.New()
	s.chNew = make(chan webRTCNewSessionReq)
	s.chAddCandidates = make(chan webRTCAddSessionCandidatesReq)
	s.chRenegotiate = make(chan webRTCRenegotiateSessionReq)

	s.Log(logger.Info, "created by %s", s.req.remoteAddr)

	s.wg.Add(1)

	go s.run()
}

// Log implements logger.Writer.
func (s *session) Log(level logger.Level, format string, args ...any) {
	id := hex.EncodeToString(s.uuid[:4])
	s.parent.Log(level, "[session %v] "+format, append([]any{id}, args...)...)
}

func (s *session) Close() {
	// Clean up track switcher
	if s.trackSwitcher != nil {
		s.trackSwitcher.Close()
	}
	s.ctxCancel()
}

func (s *session) run() {
	defer s.wg.Done()

	err := s.runInner()

	s.ctxCancel()

	s.parent.closeSession(s)

	s.Log(logger.Info, "closed: %v", err)
}

func (s *session) runInner() error {
	select {
	case <-s.chNew:
	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}

	errStatusCode, err := s.runInner2()

	if errStatusCode != 0 {
		s.req.res <- webRTCNewSessionRes{
			errStatusCode: errStatusCode,
			err:           err,
		}
	}

	return err
}

func (s *session) runInner2() (int, error) {
	// Handle renegotiation requests
	select {
	case renegReq := <-s.chRenegotiate:
		return s.handleRenegotiation(renegReq)
	default:
		// Continue with initial session setup
	}

	if s.req.publish {
		return s.runPublish()
	}
	return s.runRead()
}

func (s *session) runPublish() (int, error) {
	ip, _, _ := net.SplitHostPort(s.req.remoteAddr)

	pathConf, err := s.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:        s.req.pathName,
			Query:       s.req.httpRequest.URL.RawQuery,
			Publish:     true,
			Proto:       auth.ProtocolWebRTC,
			ID:          &s.uuid,
			Credentials: httpp.Credentials(s.req.httpRequest),
			IP:          net.ParseIP(ip),
		},
	})
	if err != nil {
		return http.StatusBadRequest, err
	}

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               false,
		Log:                   s,
	}
	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	offer := whipOffer(s.req.offer)

	var sdp sdp.SessionDescription
	err = sdp.Unmarshal([]byte(offer.SDP))
	if err != nil {
		return http.StatusBadRequest, err
	}

	err = webrtc.TracksAreValid(sdp.MediaDescriptions)
	if err != nil {
		// RFC draft-ietf-wish-whip
		// if the number of audio and or video
		// tracks or number streams is not supported by the WHIP Endpoint, it
		// MUST reject the HTTP POST request with a "406 Not Acceptable" error
		// response.
		return http.StatusNotAcceptable, err
	}

	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		return http.StatusBadRequest, err
	}

	s.writeAnswer(answer)

	go s.readRemoteCandidates(pc)

	err = pc.WaitUntilConnected()
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	err = pc.GatherIncomingTracks()
	if err != nil {
		return 0, err
	}

	var strm *stream.Stream

	medias, err := webrtc.ToStream(pc, pathConf, &strm, s)
	if err != nil {
		return 0, err
	}

	var path defs.Path
	path, strm, err = s.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author:             s,
		Desc:               &description.Session{Medias: medias},
		GenerateRTPPackets: false,
		FillNTP:            !pathConf.UseAbsoluteTimestamp,
		ConfToCompare:      pathConf,
		AccessRequest: defs.PathAccessRequest{
			Name:     s.req.pathName,
			Query:    s.req.httpRequest.URL.RawQuery,
			Publish:  true,
			SkipAuth: true,
		},
	})
	if err != nil {
		return 0, err
	}

	defer path.RemovePublisher(defs.PathRemovePublisherReq{Author: s})

	pc.StartReading()

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

// Fixed runRead() function for internal/servers/webrtc/session.go
// Replace lines 303-445 with this code
//
// This fix addresses the resource leak issue where failing sessions
// (e.g., "codecs not supported by client") don't properly clean up
// TrackSwitcher resources, eventually affecting other clients.

func (s *session) runRead() (int, error) {
	ip, _, _ := net.SplitHostPort(s.req.remoteAddr)

	req := defs.PathAccessRequest{
		Name:        s.req.pathName,
		Query:       s.req.httpRequest.URL.RawQuery,
		Proto:       auth.ProtocolWebRTC,
		ID:          &s.uuid,
		Credentials: httpp.Credentials(s.req.httpRequest),
		IP:          net.ParseIP(ip),
	}

	path, strm, err := s.pathManager.AddReader(defs.PathAddReaderReq{
		Author:        s,
		AccessRequest: req,
	})
	if err != nil {
		var terr2 defs.PathNoStreamAvailableError
		if errors.As(err, &terr2) {
			return http.StatusNotFound, err
		}

		return http.StatusBadRequest, err
	}

	defer path.RemoveReader(defs.PathRemoveReaderReq{Author: s})

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               true,
		Log:                   s,
	}

	r := &stream.Reader{Parent: s}

	// ========================================================================
	// FIX 1: Setup early cleanup for TrackSwitcher
	// This ensures cleanup happens even if we return early (e.g., CreateFullAnswer fails)
	// ========================================================================
	var trackSwitcherCreated bool
	defer func() {
		if trackSwitcherCreated && s.trackSwitcher != nil {
			// Only close if we haven't reached the success point
			// (success point is after strm.AddReader)
			s.Log(logger.Debug, "Early cleanup: closing trackSwitcher due to early return")
			s.trackSwitcher.Close()
			s.trackSwitcher = nil
		}
	}()

	useTrackSwitcher := false
	if server, ok := s.parent.(*Server); ok {
		useTrackSwitcher = server.ABREnable
		s.Log(logger.Info, "ABR Config: enabled=%v for path '%s'", useTrackSwitcher, s.req.pathName)
	}

	if useTrackSwitcher {
		s.Log(logger.Info, "WebRTC ABR enabled for path '%s', using TrackSwitcher", s.req.pathName)
		// Create and initialize track switcher for ABR
		s.trackSwitcher = webrtc.NewTrackSwitcher(s.ctx, s)
		trackSwitcherCreated = true

		// Setup tracks from stream using track switcher
		err = s.trackSwitcher.SetupFromStream(strm.Desc, r, pc, path)
		if err != nil {
			// Fallback to standard setup if track switcher fails
			s.Log(logger.Warn, "Track switcher setup failed, falling back to standard: %v", err)
			// ========================================================================
			// FIX 2: Explicitly close trackSwitcher on fallback
			// ========================================================================
			if s.trackSwitcher != nil {
				s.trackSwitcher.Close()
			}
			s.trackSwitcher = nil
			trackSwitcherCreated = false
			err = webrtc.FromStreamWithConfig(strm.Desc, r, pc, path)
		}
	} else {
		// Standard setup for non-ABR paths (按需回源拉流等)
		err = webrtc.FromStreamWithConfig(strm.Desc, r, pc, path)
	}

	if err != nil {
		return http.StatusBadRequest, err
	}

	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	offer := whipOffer(s.req.offer)

	// ========================================================================
	// FIX 3: If CreateFullAnswer fails here, cleanup happens via defer above
	// ========================================================================
	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		s.Log(logger.Warn, "CreateFullAnswer failed: %v (cleanup will be handled by defer)", err)
		return http.StatusBadRequest, err
	}

	s.writeAnswer(answer)

	go s.readRemoteCandidates(pc)

	err = pc.WaitUntilConnected()
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	s.Log(logger.Info, "is reading from path '%s', %s",
		path.Name(), defs.FormatsInfo(r.Formats()))
	models.WorkerPathManager.AddPaths(path.Name())

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          s,
		ExternalCmdPool: s.externalCmdPool,
		Conf:            path.SafeConf(),
		ExternalCmdEnv:  path.ExternalCmdEnv(),
		Reader:          s.APIReaderDescribe(),
		Query:           s.req.httpRequest.URL.RawQuery,
	})
	defer onUnreadHook()

	// ========================================================================
	// FIX 4: Success point reached - cancel early cleanup
	// From this point, normal cleanup flow takes over
	// ========================================================================
	trackSwitcherCreated = false

	strm.AddReader(r)
	defer strm.RemoveReader(r)

	// ========================================================================
	// FIX 5: Setup normal cleanup for successful session
	// This will execute when the session ends normally
	// ========================================================================
	defer func() {
		if s.trackSwitcher != nil {
			s.trackSwitcher.Close()
			s.trackSwitcher = nil
		}
	}()

	select {
	case <-pc.Failed():
		models.WorkerPathManager.DeletePath(path.Name())
		return 0, fmt.Errorf("peer connection closed")

	case err = <-r.Error():
		models.WorkerPathManager.DeletePath(path.Name())
		return 0, err

	case <-s.ctx.Done():
		models.WorkerPathManager.DeletePath(path.Name())
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) writeAnswer(answer *pwebrtc.SessionDescription) {
	s.req.res <- webRTCNewSessionRes{
		sx:     s,
		answer: []byte(answer.SDP),
	}
}

func (s *session) readRemoteCandidates(pc *webrtc.PeerConnection) {
	for {
		select {
		case req := <-s.chAddCandidates:
			for _, candidate := range req.candidates {
				err := pc.AddRemoteCandidate(candidate)
				if err != nil {
					req.res <- webRTCAddSessionCandidatesRes{err: err}
				}
			}
			req.res <- webRTCAddSessionCandidatesRes{}

		case <-s.ctx.Done():
			return
		}
	}
}

func (s *session) handleRenegotiation(req webRTCRenegotiateSessionReq) (int, error) {
	s.Log(logger.Info, "=== SDP RENEGOTIATION START ===")
	s.Log(logger.Info, "Received offer SDP (%d bytes):\n%s", len(req.offer), string(req.offer))

	// Parse the new offer SDP
	var sdp sdp.SessionDescription
	err := sdp.Unmarshal(req.offer)
	if err != nil {
		s.Log(logger.Error, "Failed to unmarshal SDP: %v", err)
		return http.StatusBadRequest, fmt.Errorf("invalid SDP: %w", err)
	}

	s.Log(logger.Debug, "Parsed SDP: %d medias", len(sdp.MediaDescriptions))

	// Extract bandwidth limit from SDP
	bandwidthLimit := s.extractBandwidthLimit(&sdp)
	s.currentBandwidthLimit = bandwidthLimit
	s.Log(logger.Info, "Extracted bandwidth limit: %d kbps", bandwidthLimit)

	// Perform track switching based on bandwidth limit if track switcher is available
	if s.trackSwitcher != nil && bandwidthLimit > 0 {
		targetTrackID := s.selectTrackForBandwidth(bandwidthLimit)
		if targetTrackID != s.currentTrackID {
			s.Log(logger.Info, "Switching track based on bandwidth limit %d kbps: %d -> %d",
				bandwidthLimit, s.currentTrackID, targetTrackID)
			if err := s.trackSwitcher.SwitchToTrack(targetTrackID); err != nil {
				s.Log(logger.Warn, "Track switch failed, continuing with current track: %v", err)
				// Continue with renegotiation even if track switch fails
			} else {
				s.currentTrackID = targetTrackID
			}
		} else {
			s.Log(logger.Debug, "No track switch needed, already on optimal track %d for bandwidth %d kbps",
				targetTrackID, bandwidthLimit)
		}
	} else if s.trackSwitcher == nil {
		s.Log(logger.Debug, "No track switcher available, skipping ABR switching")
	}

	// Perform proper WebRTC renegotiation:
	// 1. Set the new remote offer
	// 2. Create a new answer
	// 3. Return the answer

	if s.pc == nil {
		s.Log(logger.Error, "No peer connection available for renegotiation")
		return http.StatusBadRequest, fmt.Errorf("no peer connection available for renegotiation")
	}

	s.Log(logger.Info, "Peer connection available, proceeding with renegotiation")

	// Create offer from the received SDP
	offer := &pwebrtc.SessionDescription{
		Type: pwebrtc.SDPTypeOffer,
		SDP:  string(req.offer),
	}

	s.Log(logger.Info, "Setting remote description...")
	err = s.pc.SetRemoteDescriptionForRenegotiation(offer)
	if err != nil {
		s.Log(logger.Error, "Failed to set remote description: %v", err)
		return http.StatusBadRequest, fmt.Errorf("failed to set remote description: %v", err)
	}
	s.Log(logger.Info, "Remote description set successfully")

	// Create answer
	s.Log(logger.Info, "Creating answer...")
	answer, err := s.pc.CreateAnswerForRenegotiation()
	if err != nil {
		s.Log(logger.Error, "Failed to create answer: %v", err)
		return http.StatusInternalServerError, fmt.Errorf("failed to create answer: %v", err)
	}
	s.Log(logger.Info, "Answer created successfully")

	// Set local description
	s.Log(logger.Info, "Setting local description...")
	err = s.pc.SetLocalDescriptionForRenegotiation(answer)
	if err != nil {
		s.Log(logger.Error, "Failed to set local description: %v", err)
		return http.StatusInternalServerError, fmt.Errorf("failed to set local description: %v", err)
	}
	s.Log(logger.Info, "Local description set successfully")

	// Wait for ICE gathering to complete
	s.Log(logger.Info, "Waiting for ICE gathering to complete...")
	select {
	case <-s.pc.GatheringDone():
		s.Log(logger.Info, "ICE gathering completed")
	case <-time.After(2 * time.Second):
		s.Log(logger.Warn, "ICE gathering timeout during renegotiation")
	}

	// Get the final answer
	finalAnswer := s.pc.GetLocalDescription()
	if finalAnswer == nil {
		s.Log(logger.Error, "No local description available after renegotiation")
		return http.StatusInternalServerError, fmt.Errorf("no local description available")
	}
	s.Log(logger.Info, "Final answer SDP (%d bytes):\n%s", len(finalAnswer.SDP), finalAnswer.SDP)

	s.Log(logger.Info, "=== SDP RENEGOTIATION COMPLETED ===")

	req.res <- webRTCRenegotiateSessionRes{
		sx:     s,
		answer: []byte(finalAnswer.SDP),
	}

	return 0, nil
}

// new is called by webRTCHTTPServer through Server.
func (s *session) new(req webRTCNewSessionReq) webRTCNewSessionRes {
	select {
	case s.chNew <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCNewSessionRes{err: fmt.Errorf("terminated"), errStatusCode: http.StatusInternalServerError}
	}
}

// addCandidates is called by webRTCHTTPServer through Server.
func (s *session) addCandidates(
	req webRTCAddSessionCandidatesReq,
) webRTCAddSessionCandidatesRes {
	select {
	case s.chAddCandidates <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCAddSessionCandidatesRes{err: fmt.Errorf("terminated")}
	}
}

// renegotiate is called by webRTCHTTPServer through Server for SDP renegotiation.
func (s *session) renegotiate(req webRTCRenegotiateSessionReq) webRTCRenegotiateSessionRes {
	req.res = make(chan webRTCRenegotiateSessionRes)

	select {
	case s.chRenegotiate <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCRenegotiateSessionRes{err: fmt.Errorf("terminated")}
	}
}

// APIReaderDescribe implements reader.
func (s *session) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "webRTCSession",
		ID:   s.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (s *session) APISourceDescribe() defs.APIPathSourceOrReader {
	return s.APIReaderDescribe()
}

func (s *session) apiItem() *defs.APIWebRTCSession {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	peerConnectionEstablished := false
	localCandidate := ""
	remoteCandidate := ""
	bytesReceived := uint64(0)
	bytesSent := uint64(0)
	rtpPacketsReceived := uint64(0)
	rtpPacketsSent := uint64(0)
	rtpPacketsLost := uint64(0)
	rtpPacketsJitter := float64(0)
	rtcpPacketsReceived := uint64(0)
	rtcpPacketsSent := uint64(0)
	simulcastBandwidthLimit := s.currentBandwidthLimit

	if s.pc != nil {
		peerConnectionEstablished = true
		localCandidate = s.pc.LocalCandidate()
		remoteCandidate = s.pc.RemoteCandidate()
		stats := s.pc.Stats()
		bytesReceived = stats.BytesReceived
		bytesSent = stats.BytesSent
		rtpPacketsReceived = stats.RTPPacketsReceived
		rtpPacketsSent = stats.RTPPacketsSent
		rtpPacketsLost = stats.RTPPacketsLost
		rtpPacketsJitter = stats.RTPPacketsJitter
		rtcpPacketsReceived = stats.RTCPPacketsReceived
		rtcpPacketsSent = stats.RTCPPacketsSent
	}

	return &defs.APIWebRTCSession{
		ID:                        s.uuid,
		Created:                   s.created,
		RemoteAddr:                s.req.remoteAddr,
		PeerConnectionEstablished: peerConnectionEstablished,
		LocalCandidate:            localCandidate,
		RemoteCandidate:           remoteCandidate,
		State: func() defs.APIWebRTCSessionState {
			if s.req.publish {
				return defs.APIWebRTCSessionStatePublish
			}
			return defs.APIWebRTCSessionStateRead
		}(),
		Path:                    s.req.pathName,
		Query:                   s.req.httpRequest.URL.RawQuery,
		BytesReceived:           bytesReceived,
		BytesSent:               bytesSent,
		RTPPacketsReceived:      rtpPacketsReceived,
		RTPPacketsSent:          rtpPacketsSent,
		RTPPacketsLost:          rtpPacketsLost,
		RTPPacketsJitter:        rtpPacketsJitter,
		RTCPPacketsReceived:     rtcpPacketsReceived,
		RTCPPacketsSent:         rtcpPacketsSent,
		SimulcastBandwidthLimit: simulcastBandwidthLimit,
	}
}

// SetWSConnection sets the WebSocket control connection.
func (s *session) SetWSConnection(conn *WSControlConnection) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.wsConn = conn

	// Collect track information
	s.availableTracks = s.CollectTrackInfoImproved()
	s.currentTrackID = 0 // Default to first track

	s.Log(logger.Info, "WebSocket control connection established, %d tracks available", len(s.availableTracks))
}

// GetTracksInfo returns the current tracks information.
// If tracks haven't been collected yet, collect them now (lazy loading)
// This ensures tracks are available even when SetWSConnection() wasn't called
func (s *session) GetTracksInfo() wsprotocol.TracksInfoPayload {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Lazy load tracks if not already collected
	// This happens when WebSocket connects before SetWSConnection() is called
	if len(s.availableTracks) == 0 {
		s.Log(logger.Debug, "ABR: availableTracks is empty, collecting now (lazy loading)...")
		s.availableTracks = s.CollectTrackInfoImproved()
		s.currentTrackID = 0
		s.Log(logger.Info, "ABR: Lazy-loaded %d tracks for session", len(s.availableTracks))
	}

	return wsprotocol.TracksInfoPayload{
		ActiveTrackID: s.currentTrackID,
		Tracks:        s.availableTracks,
	}
}

// isHealthy checks if the session is healthy and can accept WebSocket connections.
func (s *session) isHealthy() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Check if session context is still active
	if s.ctx.Err() != nil {
		return false
	}

	// Check if we have available tracks
	if len(s.availableTracks) == 0 {
		return false
	}

	return true
}

// onWSClosed is called when WebSocket connection is closed.
func (s *session) onWSClosed() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.wsConn = nil
	s.Log(logger.Info, "WebSocket control connection closed")
}

// collectTrackInfo collects track information from the stream.
func (s *session) collectTrackInfo() []wsprotocol.TrackInfo2 {
	tracks := []wsprotocol.TrackInfo2{}

	// For now, return a basic example
	// TODO: Extract from s.stream.Desc().Medias when stream is available

	// Example tracks for Simulcast (3 video + 1 audio)
	tracks = append(tracks, wsprotocol.TrackInfo2{
		ID:      0,
		Type:    "video",
		Codec:   "h264",
		Label:   "High",
		Bitrate: 2000000,
		Width:   1920,
		Height:  1080,
	})

	tracks = append(tracks, wsprotocol.TrackInfo2{
		ID:      1,
		Type:    "video",
		Codec:   "h264",
		Label:   "Medium",
		Bitrate: 1000000,
		Width:   1280,
		Height:  720,
	})

	tracks = append(tracks, wsprotocol.TrackInfo2{
		ID:      2,
		Type:    "video",
		Codec:   "h264",
		Label:   "Low",
		Bitrate: 500000,
		Width:   960,
		Height:  540,
	})

	tracks = append(tracks, wsprotocol.TrackInfo2{
		ID:      3,
		Type:    "audio",
		Codec:   "opus",
		Label:   "audio",
		Bitrate: 128000,
	})

	s.Log(logger.Debug, "collected %d tracks", len(tracks))

	return tracks
}

// SwitchVideoTrack switches to a different track (video or audio).
func (s *session) SwitchVideoTrack(targetID int) error {
	s.trackMutex.Lock()
	defer s.trackMutex.Unlock()

	// Use track switcher if available
	if s.trackSwitcher != nil {
		s.Log(logger.Info, "Using track switcher for track change to %d", targetID)

		if err := s.trackSwitcher.SwitchToTrack(targetID); err != nil {
			s.Log(logger.Error, "Track switch failed: %v", err)
			return err
		}

		// Update session state
		oldTrackID := s.currentTrackID
		s.currentTrackID = targetID

		s.Log(logger.Info, "Track switch completed via track switcher: %d -> %d", oldTrackID, targetID)
		return nil
	}

	// Fallback to state-only update if track switcher not available
	s.Log(logger.Warn, "Track switcher not available, only updating state (no actual switch)")

	if targetID == s.currentTrackID {
		return nil
	}

	oldTrackID := s.currentTrackID
	s.currentTrackID = targetID
	s.Log(logger.Info, "State-only track switch: %d -> %d (no RTP switching)", oldTrackID, targetID)

	return nil
}

// extractBandwidthLimit extracts the bandwidth limit from SDP b=AS attribute
func (s *session) extractBandwidthLimit(sdpDesc *sdp.SessionDescription) int {
	// Look for video media section
	for _, media := range sdpDesc.MediaDescriptions {
		if media.MediaName.Media == "video" {
			// Look for b=AS attribute
			for _, attr := range media.Attributes {
				if attr.Key == "b" && strings.HasPrefix(attr.Value, "AS:") {
					parts := strings.Split(attr.Value, ":")
					if len(parts) == 2 {
						if bandwidth, err := strconv.Atoi(parts[1]); err == nil {
							return bandwidth
						}
					}
				}
			}
		}
	}
	return 0 // No bandwidth limit specified
}

// selectTrackForBandwidth selects the best track based on available bandwidth
// Enhanced to support audio-only mode for very low bandwidth (<350 kbps)
func (s *session) selectTrackForBandwidth(bandwidthKbps int) int {
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	if len(s.availableTracks) == 0 {
		s.Log(logger.Debug, "No available tracks, returning current track ID 0")
		return 0
	}

	// ✅ NEW: Audio-only mode for very low bandwidth (<350 kbps)
	// When bandwidth is critically low, switch to audio-only to maintain connection
	if bandwidthKbps < 350 {
		s.Log(logger.Info, "Bandwidth %d kbps < 350 kbps, switching to audio-only mode", bandwidthKbps)

		// Find audio track (should be the last track in the list)
		for i := len(s.availableTracks) - 1; i >= 0; i-- {
			if s.availableTracks[i].Type == "audio" {
				s.Log(logger.Info, "Switching to audio track ID: %d (codec: %s, bitrate: %d bps)",
					s.availableTracks[i].ID,
					s.availableTracks[i].Codec,
					s.availableTracks[i].Bitrate)
				return s.availableTracks[i].ID
			}
		}

		// If no audio track found, fall through to video selection
		s.Log(logger.Warn, "No audio track found for audio-only mode, using lowest video quality")
	}

	// Find the video track with highest bitrate that fits within the bandwidth limit
	bestTrackID := 0
	bestBitrate := 0

	for _, track := range s.availableTracks {
		if track.Type == "video" && track.Bitrate <= bandwidthKbps*1000 { // Convert kbps to bps
			if track.Bitrate > bestBitrate {
				bestBitrate = track.Bitrate
				bestTrackID = track.ID
			}
		}
	}

	// If no track fits the bandwidth, use the lowest bitrate video track
	if bestBitrate == 0 {
		lowestBitrate := 0
		for _, track := range s.availableTracks {
			if track.Type == "video" {
				if lowestBitrate == 0 || track.Bitrate < lowestBitrate {
					lowestBitrate = track.Bitrate
					bestTrackID = track.ID
				}
			}
		}
		bestBitrate = lowestBitrate
	}

	s.Log(logger.Debug, "Selected track %d with bitrate %d bps for bandwidth limit %d kbps",
		bestTrackID, bestBitrate, bandwidthKbps)
	return bestTrackID
}

// isValidTrackID checks if a track ID is valid for switching.
// Note: This method is kept for backward compatibility but the main validation
// is now done in SwitchVideoTrack with dynamic track collection.
func (s *session) isValidTrackID(trackID int) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// If we have cached tracks, check against them
	if len(s.availableTracks) > 0 {
		return trackID >= 0 && trackID < len(s.availableTracks)
	}

	// If no cached tracks, we can't validate here - validation happens in SwitchVideoTrack
	return trackID >= 0
}
