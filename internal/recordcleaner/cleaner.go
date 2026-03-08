// Package recordcleaner contains the recording cleaner.
package recordcleaner

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/worker/tracer"
	"github.com/bluenviron/mediamtx/worker/utils"
	"github.com/shirou/gopsutil/disk"
)

var timeNow = time.Now

// Cleaner removes expired recording segments from disk.
type Cleaner struct {
	PathConfs map[string]*conf.Path
	Parent    logger.Writer

	ctx       context.Context
	ctxCancel func()

	chReloadConf chan map[string]*conf.Path
	done         chan struct{}

	AdditionalCleanTime conf.Duration
	CheckInterval       time.Duration
}

// Initialize initializes a Cleaner.
func (c *Cleaner) Initialize() {
	c.ctx, c.ctxCancel = context.WithCancel(context.Background())
	c.chReloadConf = make(chan map[string]*conf.Path)
	c.done = make(chan struct{})

	go c.run()
}

// Close closes the Cleaner.
func (c *Cleaner) Close() {
	c.ctxCancel()
	<-c.done
}

// Log implements logger.Writer.
func (c *Cleaner) Log(level logger.Level, format string, args ...any) {
	c.Parent.Log(level, "[record cleaner]"+format, args...)
}

// ReloadPathConfs is called by core.Core.
func (c *Cleaner) ReloadPathConfs(pathConfs map[string]*conf.Path) {
	select {
	case c.chReloadConf <- pathConfs:
	case <-c.ctx.Done():
	}
}

func (c *Cleaner) run() {
	defer close(c.done)
	tracer.LogInfo(tracer.ID_APP, "cleaner cleanInterval=%v", c.cleanInterval())
	c.doRun() //nolint:errcheck

	for {
		select {
		case <-time.After(c.cleanInterval()):
			c.doRun()

		case cnf := <-c.chReloadConf:
			c.PathConfs = cnf

		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Cleaner) cleanInterval() time.Duration {
	interval := 5 * 60 * time.Second

	for _, e := range c.PathConfs {
		if e.RecordDeleteAfter != 0 &&
			interval > (time.Duration(e.RecordDeleteAfter)/2) {
			interval = time.Duration(e.RecordDeleteAfter) / 2
		}
	}

	return interval
}

func (c *Cleaner) doRun() {
	now := timeNow()

	pathNames := recordstore.FindAllPathsWithSegments(c.PathConfs)

	for _, pathName := range pathNames {
		c.processPath(now, pathName) //nolint:errcheck
	}
}

func (c *Cleaner) processPath(now time.Time, pathName string) error {
	pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName)
	if err != nil {
		return err
	}
	tracer.LogDebug(tracer.ID_APP, "cleaning pathName=%s, RecordDeleteAfter=%vh", pathName, pathConf.RecordDeleteAfter/conf.Duration(time.Hour))
	if pathConf.RecordDeleteAfter == 0 {
		return nil
	}

	err = c.deleteExpiredSegments(now, pathName, pathConf)
	if err != nil {
		tracer.LogTrace(tracer.ID_APP, "cleaner deleteExpiredSegments err=%v", err)
		//return err
	}

	c.deleteEmptyDirs(pathConf)

	return nil
}

func (c *Cleaner) deleteExpiredSegments(now time.Time, pathName string, pathConf *conf.Path) error {
	end := now.Add(-time.Duration(pathConf.RecordDeleteAfter))
	segments, err := recordstore.FindSegments(pathConf, pathName, nil, &end)
	if err != nil {
		return err
	}

	for _, seg := range segments {
		c.Log(logger.Debug, "removing %s", seg.Fpath)
		os.Remove(seg.Fpath)
	}

	return nil
}

func (c *Cleaner) deleteEmptyDirs(pathConf *conf.Path) {
	recordPath := strings.ReplaceAll(pathConf.RecordPath, "%path", pathConf.Name)
	commonPath := recordstore.CommonPath(recordPath)

	now := timeNow()
	tracer.LogDebug(tracer.ID_APP, "cleaner walks %s", commonPath)
	diskInfo, err := disk.Usage(commonPath)
	if err != nil {
		return
	}

	tracer.LogInfo(tracer.ID_APP, "%s disk usage =%0.2f%%", commonPath, diskInfo.UsedPercent)
	if diskInfo.UsedPercent >= 90 {
		c.AdditionalCleanTime = conf.Duration(5 * time.Minute)
		c.CheckInterval = 1 * 60 * time.Second
	} else if diskInfo.UsedPercent > 85 {
		c.AdditionalCleanTime = conf.Duration(35 * time.Minute)
		c.CheckInterval = 1 * 60 * time.Second
	} else if diskInfo.UsedPercent > 80 {
		c.AdditionalCleanTime = conf.Duration(65 * time.Minute)
		c.CheckInterval = 2 * 60 * time.Second
	} else if diskInfo.UsedPercent > 70 {
		c.AdditionalCleanTime = conf.Duration(95 * time.Minute)
		c.CheckInterval = 2 * 60 * time.Second
	} else {
		c.AdditionalCleanTime = pathConf.RecordDeleteAfter
		c.CheckInterval = 3 * 60 * time.Second
	}
	deleteAfterMinutes := c.AdditionalCleanTime / conf.Duration(time.Minute)
	tracer.LogInfo(tracer.ID_APP, "DeleteAfter=%v minutes.", deleteAfterMinutes)

	filepath.Walk(commonPath, func(fpath string, info fs.FileInfo, err error) error { //nolint:errcheck
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if now.Sub(info.ModTime()) > time.Duration(deleteAfterMinutes*conf.Duration(time.Minute)) {
				if utils.IsMP4(fpath) || utils.IsFLV(fpath) {
					tracer.LogDebug(tracer.ID_APP, "cleaner removing file: %s", fpath)
					os.Remove(fpath)
				}
			}
		}

		return nil
	})

	// filepath.WalkDir(commonPath, func(fpath string, info fs.DirEntry, err error) error { //nolint:errcheck
	// 	if err != nil {
	// 		return err
	// 	}

	// 	if info.IsDir() && fpath != commonPath && utils.IsDirEmpty(fpath) {
	// 		tracer.LogDebug(tracer.ID_APP, "removing dir: %s", fpath)
	// 		os.Remove(fpath)
	// 	}

	// 	return nil
	// })
}
