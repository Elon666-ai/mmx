// Package recorder contains the recorder.
package recorder

import (
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/worker/models"
	"github.com/bluenviron/mediamtx/worker/tracer"
)

const (
	ntpDriftTolerance = 5 * time.Second
)

// OnSegmentCreateFunc is the prototype of the function passed as OnSegmentCreate
type OnSegmentCreateFunc = func(path string)

// OnSegmentCompleteFunc is the prototype of the function passed as OnSegmentComplete
type OnSegmentCompleteFunc = func(path string, duration time.Duration)

// Recorder writes recordings to disk.
type Recorder struct {
	PathFormat        string
	Format            conf.RecordFormat
	PartDuration      time.Duration
	MaxPartSize       conf.StringSize
	SegmentDuration   time.Duration
	PathName          string
	Stream            *stream.Stream
	OnSegmentCreate   OnSegmentCreateFunc
	OnSegmentComplete OnSegmentCompleteFunc
	Parent            logger.Writer

	restartPause time.Duration

	currentInstance *recorderInstance

	terminate chan struct{}
	done      chan struct{}
}

var mapRecorderInstance map[string]*recorderInstance = make(map[string]*recorderInstance)
var rwlock = &sync.RWMutex{}

/*
业务逻辑：
1. game-server发送table，这个table关联的几个stream都需要同时录像。
2. recorderInstance如果存在，就通过全局变量传递录像信息(table/round/recordFile/splitFlag)
3.
[recorder.go:63]gsp2w, test00, LT0000, [test00/gsp2w-fwv]
[recorder.go:63]gsp2w, test01, LT0001, []
[recorder.go:63]gsp2w, test02, LT0002, []
*/
func MsgToRecorder(table, game, round, recordFile string) {
	var w *recorderInstance = nil
	rwlock.Lock()
	defer rwlock.Unlock()

	sids := models.GetSidList(table)
	if len(game) > 1 {
		sids = models.GetGameSidList(table, game)
	}
	tracer.LogDebug(tracer.ID_APP, "%s, %s, %s, %+v", table, game, round, sids)

	for _, sid := range sids {
		if len(game) > 1 && !strings.Contains(sid, game) {
			continue
		}

		w = mapRecorderInstance[sid]
		if w != nil {
			if w.splitFlag == 1 && len(w.recordFile) > 1 {
				tracer.LogWarn(tracer.ID_APP, "recFiles %s overwriten.(%s, %s)", w.recordFile, w.table, w.round)
			}
			w.table = table
			w.game = game
			w.round = round
			w.recordFile = recordFile
			w.splitFlag = 1
			tracer.LogInfo(tracer.ID_APP, "recorder table:%s, game=%s, round=%s, sid:%s", table, game, round, sid)
		} else {
			tracer.LogWarn(tracer.ID_APP, "recorder not found! table:%s, round=%s, sid:%s", table, round, sid)
		}
	}
}

// Initialize initializes Recorder.
func (r *Recorder) Initialize() {
	if r.OnSegmentCreate == nil {
		r.OnSegmentCreate = func(string) {
		}
	}
	if r.OnSegmentComplete == nil {
		r.OnSegmentComplete = func(string, time.Duration) {
		}
	}
	if r.restartPause == 0 {
		r.restartPause = 2 * time.Second
	}

	r.terminate = make(chan struct{})
	r.done = make(chan struct{})

	r.currentInstance = &recorderInstance{
		pathFormat:        r.PathFormat,
		format:            r.Format,
		partDuration:      r.PartDuration,
		maxPartSize:       r.MaxPartSize,
		segmentDuration:   r.SegmentDuration,
		pathName:          r.PathName,
		stream:            r.Stream,
		onSegmentCreate:   r.OnSegmentCreate,
		onSegmentComplete: r.OnSegmentComplete,
		parent:            r,
	}
	r.currentInstance.initialize()

	// protect map update and ensure old instance cleaned up
	rwlock.Lock()
	if old := mapRecorderInstance[r.PathName]; old != nil {
		// close old instance to avoid leaks (close is package-local)
		tracer.LogDebug(tracer.ID_APP, "closing previous recorderInstance for %s before replacing", r.PathName)
		old.close()
	}
	mapRecorderInstance[r.PathName] = r.currentInstance
	rwlock.Unlock()
	tracer.LogDebug(tracer.ID_APP, "new a currentInstance for %s", r.PathName)

	go r.run()
}

// Log implements logger.Writer.
func (r *Recorder) Log(level logger.Level, format string, args ...any) {
	r.Parent.Log(level, "[recorder] "+format, args...)
}

// Close closes the agent.
func (r *Recorder) Close() {
	r.Log(logger.Info, "recording stopped")
	close(r.terminate)
	<-r.done
}

func (r *Recorder) run() {
	defer close(r.done)

	for {
		select {
		case <-r.currentInstance.done:
			r.currentInstance.close()

			// remove map entry instead of setting to nil, avoid key growth
			rwlock.Lock()
			delete(mapRecorderInstance, r.PathName)
			rwlock.Unlock()
			tracer.LogDebug(tracer.ID_APP, "close currentInstance#2 for %s", r.PathName)

		case <-r.terminate:
			r.currentInstance.close()
			rwlock.Lock()
			delete(mapRecorderInstance, r.PathName)
			rwlock.Unlock()
			tracer.LogDebug(tracer.ID_APP, "close currentInstance#3 for %s", r.PathName)
			return
		}

		select {
		case <-time.After(r.restartPause):
		case <-r.terminate:
			return
		}

		r.currentInstance = &recorderInstance{
			pathFormat:        r.PathFormat,
			format:            r.Format,
			partDuration:      r.PartDuration,
			maxPartSize:       r.MaxPartSize,
			segmentDuration:   r.SegmentDuration,
			pathName:          r.PathName,
			stream:            r.Stream,
			onSegmentCreate:   r.OnSegmentCreate,
			onSegmentComplete: r.OnSegmentComplete,
			parent:            r,
		}
		r.currentInstance.initialize()
		rwlock.Lock()
		mapRecorderInstance[r.PathName] = r.currentInstance
		rwlock.Unlock()
		tracer.LogDebug(tracer.ID_APP, "new a currentInstance#3 for %s", r.PathName)
	}
}
