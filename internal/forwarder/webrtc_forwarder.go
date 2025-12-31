package forwarder

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/tls"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/protocols/whip"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// ensure webrtcForwarder implements Forwarder interface
var _ Forwarder = (*webrtcForwarder)(nil)

type webrtcForwarder struct {
	url              string
	config           *conf.WebRTCForwardTarget
	stream           *stream.Stream
	reader           *stream.Reader
	whipClient       *whip.Client
	logger           logger.Writer
	ctx              context.Context
	ctxCancel        context.CancelFunc
	wg               sync.WaitGroup
	mutex            sync.RWMutex
	writeTimeout     time.Duration
	udpReadBufferSize uint

	// statistics
	bytesSent      uint64
	packetsSent    uint64
	packetsLost    uint64
	lastError      error
	connected      bool
	reconnectCount uint64
}

// newWebRTCForwarder creates a new WebRTC forwarder.
func newWebRTCForwarder(
	url string,
	config *conf.WebRTCForwardTarget,
	parent logger.Writer,
	writeTimeout time.Duration,
	udpReadBufferSize uint,
) Forwarder {
	ctx, ctxCancel := context.WithCancel(context.Background())

	return &webrtcForwarder{
		url:              url,
		config:           config,
		logger:           parent,
		ctx:              ctx,
		ctxCancel:        ctxCancel,
		writeTimeout:     writeTimeout,
		udpReadBufferSize: udpReadBufferSize,
	}
}

// Log implements logger.Writer.
func (f *webrtcForwarder) Log(level logger.Level, format string, args ...any) {
	f.logger.Log(level, "[WebRTC forwarder %s] "+format, append([]any{f.url}, args...)...)
}

// GetTarget implements Forwarder.
func (f *webrtcForwarder) GetTarget() string {
	return f.url
}

// Start implements Forwarder.
func (f *webrtcForwarder) Start(strm *stream.Stream) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.stream != nil {
		return fmt.Errorf("forwarder already started")
	}

	f.stream = strm
	f.wg.Add(1)
	go f.run()

	return nil
}

// Stop implements Forwarder.
func (f *webrtcForwarder) Stop() {
	f.ctxCancel()
	f.wg.Wait()

	f.mutex.Lock()
	// Don't remove reader here - it's already removed by defer in runInner()
	// Removing it here would cause "close of closed channel" panic
	if f.whipClient != nil {
		f.whipClient.Close() //nolint:errcheck
	}
	f.stream = nil
	f.reader = nil
	f.whipClient = nil
	f.mutex.Unlock()
}

// IsRunning implements Forwarder.
func (f *webrtcForwarder) IsRunning() bool {
	f.mutex.RLock()
	defer f.mutex.RUnlock()
	return f.stream != nil && f.whipClient != nil
}

func (f *webrtcForwarder) run() {
	defer f.wg.Done()

	for {
		err := f.runInner()
		if err != nil {
			f.mutex.Lock()
			f.lastError = err
			f.connected = false
			f.mutex.Unlock()
			f.Log(logger.Warn, "error: %v", err)
		}

		select {
		case <-f.ctx.Done():
			return
		default:
		}

		if !f.config.Reconnect {
			return
		}

		select {
		case <-f.ctx.Done():
			return
		case <-time.After(time.Duration(f.config.ReconnectDelay)):
			atomic.AddUint64(&f.reconnectCount, 1)
			f.Log(logger.Info, "reconnecting...")
		}
	}
}

func (f *webrtcForwarder) runInner() error {
	// parse URL
	u, err := url.Parse(f.url)
	if err != nil {
		return fmt.Errorf("invalid WebRTC URL: %w", err)
	}

	// ensure scheme is http or https
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid WebRTC URL scheme: %s (must be http or https)", u.Scheme)
	}

	// ensure path ends with /whip
	if !strings.HasSuffix(u.Path, "/whip") {
		return fmt.Errorf("invalid WebRTC URL: path must end with /whip")
	}

	// log connection attempt
	f.Log(logger.Debug, "connecting to %s", u.String())

	// create HTTP client
	tr := &http.Transport{
		TLSClientConfig: tls.MakeConfig(u.Hostname(), f.config.Fingerprint),
	}
	defer tr.CloseIdleConnections()

	httpClient := &http.Client{
		Timeout:   f.writeTimeout,
		Transport: tr,
	}

	// create reader
	f.reader = &stream.Reader{Parent: f}

	// create a temporary peer connection to setup tracks from stream
	// This PC is only used to create OutgoingTracks, not for actual connection
	pc := &webrtc.PeerConnection{
		UDPReadBufferSize: f.udpReadBufferSize,
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           true,
		Log:               f,
	}

	// setup tracks from stream
	err = webrtc.FromStream(f.stream.Desc, f.reader, pc)
	if err != nil {
		return fmt.Errorf("failed to setup WebRTC tracks: %w", err)
	}

	// create WHIP client with outgoing tracks
	whipClient := &whip.Client{
		URL:               u,
		Publish:           true,
		OutgoingTracks:    pc.OutgoingTracks,
		HTTPClient:        httpClient,
		UDPReadBufferSize: f.udpReadBufferSize,
		Log:               f,
	}

	// initialize WHIP client (this will create its own PeerConnection and call setup() on tracks)
	err = whipClient.Initialize(f.ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize WHIP client: %w", err)
	}

	// add reader to stream AFTER whip client is initialized
	// This ensures that tracks are fully set up before data starts flowing
	f.stream.AddReader(f.reader)
	defer f.stream.RemoveReader(f.reader)

	f.mutex.Lock()
	f.whipClient = whipClient
	f.connected = true
	f.mutex.Unlock()

	defer func() {
		whipClient.Close() //nolint:errcheck
		f.mutex.Lock()
		f.whipClient = nil
		f.connected = false
		f.mutex.Unlock()
	}()

	// start reading from WHIP client's peer connection
	whipClient.PeerConnection().StartReading()

	// wait for connection failure or context cancellation
	errChan := make(chan error, 1)
	go func() {
		errChan <- whipClient.Wait()
	}()

	select {
	case err := <-errChan:
		return err
	case <-f.ctx.Done():
		return nil
	}
}

// GetStats implements Forwarder.
func (f *webrtcForwarder) GetStats() Stats {
	f.mutex.RLock()
	defer f.mutex.RUnlock()

	stats := Stats{
		BytesSent:      atomic.LoadUint64(&f.bytesSent),
		PacketsSent:    atomic.LoadUint64(&f.packetsSent),
		PacketsLost:    atomic.LoadUint64(&f.packetsLost),
		LastError:      f.lastError,
		Connected:      f.connected,
		ReconnectCount: atomic.LoadUint64(&f.reconnectCount),
	}

	if f.whipClient != nil && f.whipClient.PeerConnection() != nil {
		pcStats := f.whipClient.PeerConnection().Stats()
		if pcStats != nil {
			stats.BytesSent += pcStats.BytesSent
			stats.PacketsSent += pcStats.RTPPacketsSent
			stats.PacketsLost += pcStats.RTPPacketsLost
		}
	}

	return stats
}

