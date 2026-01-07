package transcoder

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// staticSource is the interface that Source implements
type staticSource interface {
	logger.Writer
	Run(defs.StaticSourceRunParams) error
	APISourceDescribe() defs.APIPathSourceOrReader
}

// Source is a transcoder static source.
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

	inputPath  string
	outputPath string
	logger     logger.Writer

	// in
	terminate chan struct{}
	done      chan struct{}
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
	inputPath, outputPath, ok := ParseTranscoderSource(conf.Source)
	if !ok {
		panic(fmt.Errorf("invalid transcoder source: %s", conf.Source))
	}

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

		inputPath:  inputPath,
		outputPath: outputPath,
		logger:     parent,

		terminate: make(chan struct{}),
		done:      make(chan struct{}),
	}

	s.logger.Log(logger.Info, "initialized transcoder source for %s -> %s", inputPath, outputPath)

	return s
}

// Log implements logger.Writer.
func (s *Source) Log(level logger.Level, format string, args ...any) {
	s.logger.Log(level, "[transcoder source %s/%s] "+format, append([]any{s.inputPath, s.outputPath}, args...)...)
}

// ParseTranscoderSource parses a transcoder source string.
// Format: "transcoder:input_path:output_path"
func ParseTranscoderSource(source string) (inputPath, outputPath string, ok bool) {
	if !strings.HasPrefix(source, "transcoder:") {
		return "", "", false
	}

	// Remove "transcoder:" prefix
	remaining := source[len("transcoder:"):]

	// Find last colon
	colonIndex := strings.LastIndexByte(remaining, ':')
	if colonIndex == -1 {
		return "", "", false
	}

	inputPath = remaining[:colonIndex]
	outputPath = remaining[colonIndex+1:]

	return inputPath, outputPath, true
}

// APISourceDescribe implements defs.Source.
func (s *Source) APISourceDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "transcoder",
		ID:   fmt.Sprintf("%s:%s", s.inputPath, s.outputPath),
	}
}

// Run implements defs.Source.
func (s *Source) Run(params defs.StaticSourceRunParams) error {
	s.Log(logger.Info, "transcoder source Run called")

	// Get the input path
	// Create a simple reader wrapper that implements defs.Reader
	readerWrapper := &transcoderReader{source: s}
	req := defs.PathAddReaderReq{
		Author: readerWrapper,
		AccessRequest: defs.PathAccessRequest{
			Name:    s.inputPath,
			Query:   "",
			Publish: false,
		},
		Res: make(chan defs.PathAddReaderRes, 1), // Buffered channel to avoid blocking
	}

	s.Log(logger.Info, "calling AddReader for input path '%s'", s.inputPath)
	// Call AddReader which returns (Path, Stream, error)
	path, _, err := s.PathManager.AddReader(req)
	if err != nil {
		s.Log(logger.Error, "AddReader returned error: %v", err)
		return fmt.Errorf("failed to add reader to input path '%s': %w", s.inputPath, err)
	}

	s.Log(logger.Info, "AddReader returned path: %v", path != nil)

	// AddReader already handled the response internally via path.addReader
	// We don't need to wait for req.Res again as it's already been consumed
	inputPath := path
	if inputPath == nil {
		s.Log(logger.Warn, "AddReader returned nil path for input path '%s'", s.inputPath)
		return fmt.Errorf("AddReader returned nil path for input path '%s'", s.inputPath)
	}

	defer func() {
		removeReq := defs.PathRemoveReaderReq{
			Author: readerWrapper,
			Res:    make(chan struct{}, 1), // Buffered channel to avoid blocking
		}
		s.PathManager.RemoveReader(removeReq)
		// Use select with timeout to avoid blocking forever
		select {
		case <-removeReq.Res:
		case <-time.After(1 * time.Second):
			s.Log(logger.Warn, "timeout waiting for RemoveReader response")
		}
	}()

	// Get transcoder output stream from input path
	// Use reflection to call GetTranscoderOutputStream
	s.Log(logger.Info, "calling GetTranscoderOutputStream for output path '%s'", s.outputPath)
	inputPathVal := reflect.ValueOf(inputPath)
	getTranscoderMethod := inputPathVal.MethodByName("GetTranscoderOutputStream")
	if !getTranscoderMethod.IsValid() {
		return fmt.Errorf("input path '%s' does not have GetTranscoderOutputStream method", s.inputPath)
	}

	results := getTranscoderMethod.Call([]reflect.Value{reflect.ValueOf(s.outputPath)})
	if len(results) != 1 {
		return fmt.Errorf("GetTranscoderOutputStream returned unexpected number of results")
	}

	outputStreamVal := results[0]
	if outputStreamVal.IsNil() {
		s.Log(logger.Warn, "GetTranscoderOutputStream returned nil for output path '%s', transcoder may not be ready yet", s.outputPath)
		return fmt.Errorf("transcoder output stream '%s' not found for path '%s'", s.outputPath, s.inputPath)
	}

	outputStream := outputStreamVal.Interface().(*stream.Stream)

	s.Log(logger.Info, "successfully connected to transcoder output stream %s/%s", s.inputPath, s.outputPath)

	// Check if stream already has a description (set during creation)
	if outputStream.Desc != nil && len(outputStream.Desc.Medias) > 0 {
		s.Log(logger.Info, "transcoder output stream already has description with %d medias", len(outputStream.Desc.Medias))
	} else {
		s.Log(logger.Debug, "transcoder output stream Desc is nil or empty, waiting for it to be set...")
		// Wait for the output stream to have a description
		// The stream should have a Desc set when created, but we wait a bit to ensure
		// the transcoder has started and the Desc is properly initialized
		maxWaitTime := 5 * time.Second
		waitStart := time.Now()
		for outputStream.Desc == nil || len(outputStream.Desc.Medias) == 0 {
			select {
			case <-params.Context.Done():
				return fmt.Errorf("context cancelled while waiting for transcoder output stream")
			default:
			}
			if time.Since(waitStart) > maxWaitTime {
				s.Log(logger.Warn, "transcoder output stream '%s' did not become ready within %v, using initial description if available", s.outputPath, maxWaitTime)
				// If we timeout, check if we have an initial description to use
				if outputStream.Desc == nil {
					return fmt.Errorf("transcoder output stream '%s' has no description after %v", s.outputPath, maxWaitTime)
				}
				// Use whatever description we have, even if medias is empty
				break
			}
			select {
			case <-params.Context.Done():
				return fmt.Errorf("context cancelled while waiting for transcoder output stream")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	// Notify the path system that the stream is ready
	setReadyRes := s.Parent.SetReady(defs.PathSourceStaticSetReadyReq{
		Desc:               outputStream.Desc,
		GenerateRTPPackets: true,
		FillNTP:            true,
	})
	if setReadyRes.Err != nil {
		return fmt.Errorf("failed to set transcoder source ready: %w", setReadyRes.Err)
	}

	defer s.Parent.SetNotReady(defs.PathSourceStaticSetNotReadyReq{})

	// The transcoder source acts as a reader for the transcoded output stream
	// Create a simple reader that just keeps the connection alive
	reader := &stream.Reader{Parent: s.logger}
	outputStream.AddReader(reader)
	defer outputStream.RemoveReader(reader)

	s.Log(logger.Info, "transcoder source ready, stream description: %s", defs.MediasInfo(outputStream.Desc.Medias))

	// Keep the source running until terminated
	// Listen to both terminate channel and context cancellation
	select {
	case <-s.terminate:
		s.Log(logger.Info, "stopped by terminate signal")
	case <-params.Context.Done():
		s.Log(logger.Info, "stopped by context cancellation")
	}

	close(s.done)
	return nil
}

// Stop implements defs.Source.
func (s *Source) Stop() {
	close(s.terminate)
	<-s.done
}

// transcoderReader is a wrapper that implements defs.Reader
type transcoderReader struct {
	source *Source
}

func (r *transcoderReader) Close() {
	// Nothing to close
}

func (r *transcoderReader) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "transcoder",
		ID:   fmt.Sprintf("%s:%s", r.source.inputPath, r.source.outputPath),
	}
}
