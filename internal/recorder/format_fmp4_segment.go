package recorder

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/worker/tasks"
	"github.com/bluenviron/mediamtx/worker/tracer"
	"github.com/bluenviron/mediamtx/worker/utils"
	"github.com/google/uuid"
)

func writeInit(
	f io.Writer,
	streamID uuid.UUID,
	segmentNumber uint64,
	dts time.Duration,
	ntp time.Time,
	tracks []*formatFMP4Track,
) error {
	fmp4Tracks := make([]*fmp4.InitTrack, len(tracks))
	for i, track := range tracks {
		fmp4Tracks[i] = track.initTrack
	}

	init := fmp4.Init{
		Tracks: fmp4Tracks,
		UserData: []amp4.IBox{
			&recordstore.Mtxi{
				FullBox: amp4.FullBox{
					Version: 0,
				},
				StreamID:      streamID,
				SegmentNumber: segmentNumber,
				DTS:           int64(dts),
				NTP:           ntp.UnixNano(),
			},
		},
	}

	var buf seekablebuffer.Buffer
	err := init.Marshal(&buf)
	if err != nil {
		return err
	}

	_, err = f.Write(buf.Bytes())
	return err
}

/*
目前MP4文件中的结构是这样的：
[ftyp][mdat: 视频数据][moov: 元数据]
默认情况下，moov 是在文件结尾，播放器必须等整个文件加载完，才能播放。
使用 faststart 后，结构变为：
[ftyp][moov][mdat]，播放器可以边下边播，适用于网页或流媒体环境（如 HTML5 <video> 标签）。
*/
func writeDuration(f io.ReadWriteSeeker, d time.Duration) error {
	_, err := f.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	// check and skip ftyp header and content

	buf := make([]byte, 8)
	_, err = io.ReadFull(f, buf)
	if err != nil {
		return err
	}

	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return fmt.Errorf("ftyp box not found")
	}

	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	_, err = f.Seek(int64(ftypSize), io.SeekStart)
	if err != nil {
		return err
	}

	// check and skip moov header

	_, err = io.ReadFull(f, buf)
	if err != nil {
		return err
	}

	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return fmt.Errorf("moov box not found")
	}

	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	moovPos, err := f.Seek(8, io.SeekCurrent)
	if err != nil {
		return err
	}

	var mvhd amp4.Mvhd
	_, err = amp4.Unmarshal(f, uint64(moovSize-8), &mvhd, amp4.Context{})
	if err != nil {
		return err
	}

	mvhd.DurationV0 = uint32(d / time.Millisecond)

	_, err = f.Seek(moovPos, io.SeekStart)
	if err != nil {
		return err
	}

	_, err = amp4.Marshal(f, &mvhd, amp4.Context{})
	if err != nil {
		return err
	}

	return nil
}

type formatFMP4Segment struct {
	f        *formatFMP4
	startDTS time.Duration
	startNTP time.Time
	number   uint64

	path           string
	fi             *os.File
	curPart        *formatFMP4Part
	endDTS         time.Duration
	nextPartNumber uint32
}

func (s *formatFMP4Segment) initialize() {
	s.endDTS = s.startDTS
}

/*
2026-02-04 17:41:27.122985 closing segment ./nginx/html/20260204/recordings/demo01/h264_720p_1mbps-fwv.20260204174035.mp4, code=round0001-demo01, isUpload=true
*/
func (s *formatFMP4Segment) close(code string, isUpload bool) error {
	var err error = nil

	if s.curPart != nil {
		err = s.closeCurPart()
	}

	if s.fi != nil {
		tracer.LogDebug(tracer.ID_APP, "closing segment %s, code=%s, isUpload=%v", s.path, code, isUpload)

		// write overall duration in the header to speed up the playback server
		duration := s.endDTS - s.startDTS
		err2 := writeDuration(s.fi, duration)
		if err == nil {
			err = err2
		}

		err2 = s.fi.Close()
		if err == nil {
			err = err2
		}

		if err2 == nil {
			s.f.ri.onSegmentComplete(s.path, duration)
		}

		if len(code) > 1 {
			newpath := utils.GetNewFilepath(s.path, code)
			tracer.LogDebug(tracer.ID_APP, "movflags: %s --> %s", s.path, newpath)
			ex := exec.Command("ffmpeg", "-loglevel", "fatal", "-i", s.path, "-movflags", "faststart", "-c", "copy", "-f", "mp4", newpath)
			err = ex.Run()
			os.Remove(s.path)

			var dur int = int(time.Now().UnixMilli() - s.f.ri.splitStartTS)
			fInfo, err := os.Stat(newpath)
			if isUpload && err == nil && fInfo.Size() > 1024 {
				tasks.MsgToUploader(utils.S3_UPLOAD_RECORD_FILE, newpath, s.f.ri.game, s.f.ri.round, dur/1000)
				tracer.LogDebug(tracer.ID_APP, "save-record: %s, size=%d, dur=%d ms", filepath.Base(newpath), fInfo.Size(), dur)
			}
		}
	}

	return err
}

func (s *formatFMP4Segment) closeCurPart() error {
	if s.fi == nil {
		s.path = recordstore.Path{Start: s.startNTP}.Encode(s.f.ri.pathFormat2)
		tracer.LogDebug(tracer.ID_APP, "creating segment %s", s.path)

		err := os.MkdirAll(filepath.Dir(s.path), 0o755)
		if err != nil {
			return err
		}

		fi, err := os.Create(s.path)
		if err != nil {
			return err
		}

		s.f.ri.onSegmentCreate(s.path)

		err = writeInit(
			fi,
			s.f.ri.streamID,
			s.number,
			s.startDTS,
			s.startNTP,
			s.f.tracks)
		if err != nil {
			fi.Close()
			return err
		}

		s.fi = fi
	}

	return s.curPart.close(s.fi)
}

func (s *formatFMP4Segment) write(track *formatFMP4Track, sample *formatFMP4Sample, dts time.Duration) error {
	endDTS := dts + timestampToDuration(int64(sample.Duration), int(track.initTrack.TimeScale))
	if endDTS > s.endDTS {
		s.endDTS = endDTS
	}

	if s.curPart == nil {
		s.curPart = &formatFMP4Part{
			maxPartSize:     s.f.ri.maxPartSize,
			segmentStartDTS: s.startDTS,
			number:          s.nextPartNumber,
			startDTS:        dts,
		}
		s.curPart.initialize()
		s.nextPartNumber++
	} else if s.curPart.duration() >= s.f.ri.partDuration {
		err := s.closeCurPart()
		s.curPart = nil

		if err != nil {
			return err
		}

		s.curPart = &formatFMP4Part{
			maxPartSize:     s.f.ri.maxPartSize,
			segmentStartDTS: s.startDTS,
			number:          s.nextPartNumber,
			startDTS:        dts,
		}
		s.curPart.initialize()
		s.nextPartNumber++
	}

	return s.curPart.write(track, sample, dts)
}
