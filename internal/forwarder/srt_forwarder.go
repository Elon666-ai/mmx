package forwarder

import (
	"bufio"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	srt "github.com/datarhei/gosrt"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/mpegts"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// srtForwarder is a SRT forwarder implementation.
type srtForwarder struct {
	url              string
	config           *conf.SRTForwardTarget
	stream           *stream.Stream
	reader           *stream.Reader
	sconn            srt.Conn
	logger           logger.Writer
	ctx              context.Context
	ctxCancel        context.CancelFunc
	wg               sync.WaitGroup
	mutex            sync.RWMutex
	writeTimeout     time.Duration
	udpMaxPayloadSize int

	// statistics
	bytesSent      uint64
	packetsSent    uint64
	packetsLost    uint64
	lastError      error
	connected      bool
	reconnectCount uint64
}

// newSRTForwarder creates a new SRT forwarder.
func newSRTForwarder(
	url string,
	config *conf.SRTForwardTarget,
	parent logger.Writer,
	writeTimeout time.Duration,
	udpMaxPayloadSize int,
) Forwarder {
	ctx, ctxCancel := context.WithCancel(context.Background())

	return &srtForwarder{
		url:              url,
		config:           config,
		logger:           parent,
		ctx:              ctx,
		ctxCancel:        ctxCancel,
		writeTimeout:     writeTimeout,
		udpMaxPayloadSize: udpMaxPayloadSize,
	}
}

// Start starts the forwarder.
func (f *srtForwarder) Start(strm *stream.Stream) error {
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

// Stop stops the forwarder.
func (f *srtForwarder) Stop() {
	f.ctxCancel()
	f.wg.Wait()

	f.mutex.Lock()
	// Note: reader is removed by defer in runInner(), so we don't remove it here
	// to avoid "close of closed channel" panic
	if f.sconn != nil {
		f.sconn.Close()
	}
	f.stream = nil
	f.reader = nil
	f.sconn = nil
	f.mutex.Unlock()
}

// IsRunning returns whether the forwarder is running.
func (f *srtForwarder) IsRunning() bool {
	f.mutex.RLock()
	defer f.mutex.RUnlock()
	return f.stream != nil
}

// GetStats returns statistics.
func (f *srtForwarder) GetStats() Stats {
	f.mutex.RLock()
	defer f.mutex.RUnlock()

	var lastErr error
	if f.lastError != nil {
		lastErr = f.lastError
	}

	return Stats{
		BytesSent:     atomic.LoadUint64(&f.bytesSent),
		PacketsSent:   atomic.LoadUint64(&f.packetsSent),
		PacketsLost:   atomic.LoadUint64(&f.packetsLost),
		LastError:     lastErr,
		Connected:     f.connected,
		ReconnectCount: atomic.LoadUint64(&f.reconnectCount),
	}
}

// GetTarget returns the target URL.
func (f *srtForwarder) GetTarget() string {
	return f.url
}

func (f *srtForwarder) run() {
	defer f.wg.Done()

	for {
		select {
		case <-f.ctx.Done():
			return
		default:
			err := f.runInner()
			if err != nil {
				f.mutex.Lock()
				f.lastError = err
				f.connected = false
				f.mutex.Unlock()

				f.logger.Log(logger.Warn, "SRT forwarder error: %v", err)

				if f.config.Reconnect {
					atomic.AddUint64(&f.reconnectCount, 1)
					time.Sleep(time.Duration(f.config.ReconnectDelay))
					continue
				}
				return
			}
		}
	}
}

func (f *srtForwarder) runInner() error {
	// parse URL
	srtConf := srt.DefaultConfig()
	address, err := srtConf.UnmarshalURL(f.url)
	if err != nil {
		return fmt.Errorf("invalid SRT URL: %w", err)
	}
	
	// log connection attempt
	f.logger.Log(logger.Debug, "SRT forwarder: connecting to %s (streamid: %s)", address, srtConf.StreamId)

	// configure SRT
	if f.config.Passphrase != "" {
		srtConf.Passphrase = f.config.Passphrase
	}
	
	// Note: streamid is already extracted from URL by UnmarshalURL
	// and stored in srtConf.StreamId, so we don't need to replace it again
	if f.config.Latency > 0 {
		srtConf.Latency = time.Duration(f.config.Latency) * time.Millisecond
	} else {
		srtConf.Latency = 120 * time.Millisecond // default
	}
	if f.config.PacketSize > 0 {
		srtConf.PayloadSize = uint32(f.config.PacketSize)
	} else {
		srtConf.PayloadSize = 1316 // default
	}

	err = srtConf.Validate()
	if err != nil {
		return fmt.Errorf("invalid SRT config: %w", err)
	}

	// establish SRT connection
	sconn, err := srt.Dial("srt", address, srtConf)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	f.mutex.Lock()
	f.sconn = sconn
	f.connected = true
	f.mutex.Unlock()

	defer func() {
		sconn.Close()
		f.mutex.Lock()
		f.sconn = nil
		f.connected = false
		f.mutex.Unlock()
	}()

	// create buffered writer
	maxPayloadSize := f.srtMaxPayloadSize()
	bw := bufio.NewWriterSize(sconn, maxPayloadSize)

	// create reader
	f.reader = &stream.Reader{Parent: f.logger}

	// use mpegts.FromStream to convert Stream to MPEG-TS and send via SRT
	err = mpegts.FromStream(f.stream.Desc, f.reader, bw, sconn, f.writeTimeout)
	if err != nil {
		return fmt.Errorf("failed to setup MPEG-TS writer: %w", err)
	}

	// add reader to stream
	f.stream.AddReader(f.reader)
	defer f.stream.RemoveReader(f.reader)

	// wait for connection failure or context cancellation
	// the connection will be monitored by the reader's error channel
	errChan := make(chan error, 1)
	go func() {
		// wait for reader error
		err := <-f.reader.Error()
		errChan <- err
	}()

	// wait for error or context cancellation
	select {
	case err := <-errChan:
		return err
	case <-f.ctx.Done():
		return nil
	}
}

func (f *srtForwarder) srtMaxPayloadSize() int {
	// calculate max payload size
	// SRT header = 16 bytes, MPEG-TS packet = 188 bytes
	return ((f.udpMaxPayloadSize - 16) / 188) * 188
}

