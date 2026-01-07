package simulcast

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

// staticSource is the interface that Source implements
type staticSource interface {
	logger.Writer
	Run(defs.StaticSourceRunParams) error
	APISourceDescribe() defs.APIPathSourceOrReader
}

// Source is a Simulcast static source.
type Source struct {
	Conf              *conf.Path
	LogLevel          conf.LogLevel
	ReadTimeout       conf.Duration
	WriteTimeout      conf.Duration
	WriteQueueSize    int
	UDPReadBufferSize uint
	RTPMaxPayloadSize int
	Matches           []string
	PathManager       interface {
		AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error)
		RemoveReader(req defs.PathRemoveReaderReq)
	}
	Parent interface {
		logger.Writer
		SetReady(req defs.PathSourceStaticSetReadyReq) defs.PathSourceStaticSetReadyRes
		SetNotReady(req defs.PathSourceStaticSetNotReadyReq)
	}

	config      *conf.SimulcastConfig
	logger      logger.Writer

	// Output stream (the path's stream that clients read from)
	outputStream *stream.Stream

	// Input streams and readers
	inputStreams map[string]*stream.Stream // path -> stream
	readers      map[string]*stream.Reader  // path -> reader

	// Layer mapping
	layerMapping map[string]*layerInfo // path -> layer info

	// Context
	ctx       context.Context
	ctxCancel context.CancelFunc
	wg        sync.WaitGroup

	// State
	active bool
	mutex   sync.RWMutex
}

// layerInfo stores layer-related information
type layerInfo struct {
	Layer      string // "high", "medium", "low"
	SSRC       uint32 // Simulcast layer's SSRC
	RID        string // RTP Stream Identifier
	Resolution string // Resolution
	Bitrate    uint   // Bitrate
}

// New allocates a Source.
func New(
	conf *conf.Path,
	logLevel conf.LogLevel,
	readTimeout conf.Duration,
	writeTimeout conf.Duration,
	writeQueueSize int,
	udpReadBufferSize uint,
	rtpMaxPayloadSize int,
	matches []string,
	pathManager interface {
		AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error)
		RemoveReader(req defs.PathRemoveReaderReq)
	},
	parent interface {
		logger.Writer
		SetReady(req defs.PathSourceStaticSetReadyReq) defs.PathSourceStaticSetReadyRes
		SetNotReady(req defs.PathSourceStaticSetNotReadyReq)
	},
) staticSource {
	ctx, ctxCancel := context.WithCancel(context.Background())

	s := &Source{
		Conf:              conf,
		LogLevel:          logLevel,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		WriteQueueSize:    writeQueueSize,
		UDPReadBufferSize: udpReadBufferSize,
		RTPMaxPayloadSize: rtpMaxPayloadSize,
		Matches:           matches,
		PathManager:       pathManager,
		Parent:            parent,

		config:       conf.SimulcastConfig,
		logger:       parent,
		inputStreams: make(map[string]*stream.Stream),
		readers:      make(map[string]*stream.Reader),
		layerMapping: make(map[string]*layerInfo),

		ctx:       ctx,
		ctxCancel: ctxCancel,
	}

	s.logger.Log(logger.Info, "initialized simulcast source with %d inputs", len(s.config.Inputs))

	return s
}

// Log implements logger.Writer.
func (s *Source) Log(level logger.Level, format string, args ...any) {
	s.logger.Log(level, "[simulcast source] "+format, args...)
}

// APISourceDescribe implements defs.Source.
func (s *Source) APISourceDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "simulcast",
		ID:   fmt.Sprintf("simulcast:%d_inputs", len(s.config.Inputs)),
	}
}

// randUint32 generates a random uint32 for SSRC
func randUint32() (uint32, error) {
	var b [4]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return 0, err
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}

// Run implements defs.Source.
func (s *Source) Run(params defs.StaticSourceRunParams) error {
	s.Log(logger.Info, "simulcast source Run called")

	// Step 1: Connect to all input paths
	if err := s.connectInputs(); err != nil {
		return fmt.Errorf("failed to connect inputs: %w", err)
	}
	defer s.disconnectInputs()

	// Step 2: Notify path system that stream is ready
	desc := s.createStreamDescription()
	res := s.Parent.SetReady(defs.PathSourceStaticSetReadyReq{
		Desc:               desc,
		GenerateRTPPackets: true,
		FillNTP:            true,
	})
	if res.Err != nil {
		return fmt.Errorf("failed to set ready: %w", res.Err)
	}
	defer s.Parent.SetNotReady(defs.PathSourceStaticSetNotReadyReq{})

	// Store the output stream
	s.outputStream = res.Stream

	// Step 3: Start data forwarding (write to output stream)
	if err := s.startDataForwarding(); err != nil {
		return fmt.Errorf("failed to start data forwarding: %w", err)
	}

	s.Log(logger.Info, "simulcast source ready")

	// Step 6: Wait for termination
	select {
	case <-s.ctx.Done():
		s.Log(logger.Info, "stopped by context cancellation")
	case <-params.Context.Done():
		s.Log(logger.Info, "stopped by params context cancellation")
	}

	return nil
}

// connectInputs connects to all input paths
func (s *Source) connectInputs() error {
	s.Log(logger.Info, "connecting to %d input paths", len(s.config.Inputs))

	for _, input := range s.config.Inputs {
		s.Log(logger.Debug, "connecting to input path: %s", input.Path)

		// Create reader wrapper
		readerWrapper := &simulcastReader{source: s, path: input.Path}
		req := defs.PathAddReaderReq{
			Author: readerWrapper,
			AccessRequest: defs.PathAccessRequest{
				Name:    input.Path,
				Query:   "",
				Publish: false,
			},
			Res: make(chan defs.PathAddReaderRes, 1),
		}

		// Call AddReader
		path, strm, err := s.PathManager.AddReader(req)
		if err != nil {
			return fmt.Errorf("failed to add reader to path '%s': %w", input.Path, err)
		}

		if path == nil || strm == nil {
			return fmt.Errorf("path '%s' returned nil", input.Path)
		}

		// Wait for stream to be ready
		maxWaitTime := 10 * time.Second
		waitStart := time.Now()
		for strm.Desc == nil || len(strm.Desc.Medias) == 0 {
			select {
			case <-s.ctx.Done():
				return fmt.Errorf("context cancelled while waiting for stream")
			case <-time.After(100 * time.Millisecond):
			}
			if time.Since(waitStart) > maxWaitTime {
				return fmt.Errorf("path '%s' did not become ready within %v", input.Path, maxWaitTime)
			}
		}

		// Create reader
		reader := &stream.Reader{Parent: s.logger}
		strm.AddReader(reader)

		s.inputStreams[input.Path] = strm
		s.readers[input.Path] = reader

		// Create layer info
		layerInfo := &layerInfo{
			Layer:      input.Layer,
			Resolution: input.Resolution,
			Bitrate:    input.Bitrate,
			RID:        input.Layer,
		}

		// Generate SSRC for this layer
		ssrc, err := randUint32()
		if err != nil {
			return fmt.Errorf("failed to generate SSRC for path '%s': %w", input.Path, err)
		}
		layerInfo.SSRC = ssrc

		s.layerMapping[input.Path] = layerInfo

		s.Log(logger.Info, "connected to input path: %s, medias: %s, SSRC: %d",
			input.Path, defs.MediasInfo(strm.Desc.Medias), ssrc)
	}

	return nil
}

// disconnectInputs disconnects from all input paths
func (s *Source) disconnectInputs() {
	for path, reader := range s.readers {
		if strm, ok := s.inputStreams[path]; ok && reader != nil {
			strm.RemoveReader(reader)
			s.Log(logger.Debug, "disconnected from input path: %s", path)
		}
	}

	// Remove readers via PathManager
	for path := range s.readers {
		removeReq := defs.PathRemoveReaderReq{
			Author: &simulcastReader{source: s, path: path},
			Res:    make(chan struct{}, 1),
		}
		s.PathManager.RemoveReader(removeReq)
		select {
		case <-removeReq.Res:
		case <-time.After(1 * time.Second):
			s.Log(logger.Warn, "timeout waiting for RemoveReader response for path: %s", path)
		}
	}

	s.inputStreams = make(map[string]*stream.Stream)
	s.readers = make(map[string]*stream.Reader)
}


// createStreamDescription creates a stream description for the Simulcast output
func (s *Source) createStreamDescription() *description.Session {
	desc := &description.Session{}

	// Find first video input to get video format
	for _, input := range s.config.Inputs {
		if input.Type == "video" {
			if strm, ok := s.inputStreams[input.Path]; ok && strm.Desc != nil {
				// Find H.264 format
				for _, media := range strm.Desc.Medias {
					if media.Type == description.MediaTypeVideo {
						for _, fmt := range media.Formats {
							if h264, ok := fmt.(*format.H264); ok {
								videoMedia := &description.Media{
									Type: description.MediaTypeVideo,
									Formats: []format.Format{h264},
								}
								desc.Medias = append(desc.Medias, videoMedia)
								break
							}
						}
					}
				}
			}
			break
		}
	}

	// Find audio input to get audio format
	for _, input := range s.config.Inputs {
		if input.Type == "audio" {
			if strm, ok := s.inputStreams[input.Path]; ok && strm.Desc != nil {
				// Find Opus format
				for _, media := range strm.Desc.Medias {
					if media.Type == description.MediaTypeAudio {
						for _, fmt := range media.Formats {
							if opus, ok := fmt.(*format.Opus); ok {
								audioMedia := &description.Media{
									Type: description.MediaTypeAudio,
									Formats: []format.Format{opus},
								}
								desc.Medias = append(desc.Medias, audioMedia)
								break
							}
						}
					}
				}
			}
			break
		}
	}

	return desc
}

// simulcastReader is a wrapper that implements defs.Reader
type simulcastReader struct {
	source *Source
	path   string
}

func (r *simulcastReader) Close() {
	// Nothing to close
}

func (r *simulcastReader) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "simulcast",
		ID:   r.path,
	}
}


// startDataForwarding starts forwarding data from input streams
func (s *Source) startDataForwarding() error {
	s.Log(logger.Info, "starting data forwarding")

	// Start forwarding goroutine for each input stream
	for i := range s.config.Inputs {
		input := &s.config.Inputs[i]
		s.wg.Add(1)
		go s.forwardInput(input)
	}

	return nil
}

// forwardInput forwards data from a single input
func (s *Source) forwardInput(input *conf.SimulcastInput) {
	defer s.wg.Done()

	strm := s.inputStreams[input.Path]
	reader := s.readers[input.Path]
	layerInfo := s.layerMapping[input.Path]

	if strm == nil || reader == nil {
		s.Log(logger.Error, "input stream or reader not found for path: %s", input.Path)
		return
	}

	s.Log(logger.Info, "starting forward for input: %s, layer: %s", input.Path, layerInfo.Layer)

	// Forward based on type
	if input.Type == "video" {
		s.forwardVideo(strm, reader, input, layerInfo)
	} else {
		s.forwardAudio(strm, reader, input)
	}
}

// forwardVideo forwards video RTP packets
func (s *Source) forwardVideo(
	strm *stream.Stream,
	reader *stream.Reader,
	input *conf.SimulcastInput,
	layerInfo *layerInfo,
) {
	// Find video media
	var videoMedia *description.Media
	var h264Format *format.H264

	for _, media := range strm.Desc.Medias {
		if media.Type == description.MediaTypeVideo {
			videoMedia = media
			for _, fmt := range media.Formats {
				if h264, ok := fmt.(*format.H264); ok {
					h264Format = h264
					break
				}
			}
			break
		}
	}

	if videoMedia == nil || h264Format == nil {
		s.Log(logger.Error, "video media or H.264 format not found")
		return
	}

	s.Log(logger.Info, "setting up video forward for path: %s, layer: %s, SSRC: %d",
		input.Path, layerInfo.Layer, layerInfo.SSRC)

	// Set up data callback
	reader.OnData(videoMedia, h264Format, func(u *unit.Unit) error {
		select {
		case <-s.ctx.Done():
			return fmt.Errorf("context cancelled")
		default:
		}

		if u.NilPayload() {
			return nil
		}

		// Process RTP packets
		for _, originalPkt := range u.RTPPackets {
			// Clone RTP packet (avoid modifying original)
			pkt := &rtp.Packet{
				Header:  originalPkt.Header,
				Payload: make([]byte, len(originalPkt.Payload)),
			}
			copy(pkt.Payload, originalPkt.Payload)

			// Modify SSRC for Simulcast layer
			pkt.SSRC = layerInfo.SSRC

			// Write to output stream (path's stream that clients read from)
			// Find the video media in output stream
			var outputVideoMedia *description.Media
			var outputH264Format *format.H264
			for _, media := range s.outputStream.Desc.Medias {
				if media.Type == description.MediaTypeVideo {
					outputVideoMedia = media
					for _, fmt := range media.Formats {
						if h264, ok := fmt.(*format.H264); ok {
							outputH264Format = h264
							break
						}
					}
					break
				}
			}

			if outputVideoMedia != nil && outputH264Format != nil {
				// Calculate PTS from RTP timestamp
				pts := int64(pkt.Timestamp)
				s.outputStream.WriteRTPPacket(outputVideoMedia, outputH264Format, pkt, u.NTP, pts)
			} else {
				s.Log(logger.Warn, "output stream video media not found")
			}
		}

		return nil
	})

	s.Log(logger.Info, "video forward started for path: %s", input.Path)

	// Wait for context cancellation
	<-s.ctx.Done()

	s.Log(logger.Info, "video forward stopped for path: %s", input.Path)
}

// forwardAudio forwards audio RTP packets
func (s *Source) forwardAudio(
	strm *stream.Stream,
	reader *stream.Reader,
	input *conf.SimulcastInput,
) {
	// Find audio media
	var audioMedia *description.Media
	var opusFormat *format.Opus

	for _, media := range strm.Desc.Medias {
		if media.Type == description.MediaTypeAudio {
			audioMedia = media
			for _, fmt := range media.Formats {
				if opus, ok := fmt.(*format.Opus); ok {
					opusFormat = opus
					break
				}
			}
			break
		}
	}

	if audioMedia == nil || opusFormat == nil {
		s.Log(logger.Error, "audio media or Opus format not found")
		return
	}

	s.Log(logger.Info, "setting up audio forward for path: %s", input.Path)

	// Set up data callback
	reader.OnData(audioMedia, opusFormat, func(u *unit.Unit) error {
		select {
		case <-s.ctx.Done():
			return fmt.Errorf("context cancelled")
		default:
		}

		if u.NilPayload() {
			return nil
		}

		// Process RTP packets
		for _, originalPkt := range u.RTPPackets {
			// Clone RTP packet
			pkt := &rtp.Packet{
				Header:  originalPkt.Header,
				Payload: make([]byte, len(originalPkt.Payload)),
			}
			copy(pkt.Payload, originalPkt.Payload)

			// Write to output stream (path's stream that clients read from)
			// Find the audio media in output stream
			var outputAudioMedia *description.Media
			var outputOpusFormat *format.Opus
			for _, media := range s.outputStream.Desc.Medias {
				if media.Type == description.MediaTypeAudio {
					outputAudioMedia = media
					for _, fmt := range media.Formats {
						if opus, ok := fmt.(*format.Opus); ok {
							outputOpusFormat = opus
							break
						}
					}
					break
				}
			}

			if outputAudioMedia != nil && outputOpusFormat != nil {
				// Calculate PTS from RTP timestamp
				pts := int64(pkt.Timestamp)
				s.outputStream.WriteRTPPacket(outputAudioMedia, outputOpusFormat, pkt, u.NTP, pts)
			} else {
				s.Log(logger.Warn, "output stream audio media not found")
			}
		}

		return nil
	})

	s.Log(logger.Info, "audio forward started for path: %s", input.Path)

	// Wait for context cancellation
	<-s.ctx.Done()

	s.Log(logger.Info, "audio forward stopped for path: %s", input.Path)
}

