package main

import (
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/richardwilkes/toolbox/cmdline"
	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/fatal"
	"github.com/richardwilkes/toolbox/log/tracelog"
	"github.com/richardwilkes/toolbox/xio/fs"
	"github.com/richardwilkes/torrent"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
)

func main() {
	cmdline.AppName = "Simple Torrent"
	cmdline.AppCmdName = "torrent"
	cmdline.License = "Mozilla Public License, version 2.0"
	cmdline.CopyrightStartYear = "2017"
	cmdline.CopyrightHolder = "Richard A. Wilkes"
	cmdline.AppIdentifier = "com.trollworks.torrent"

	downloadCap := 300 * 1024 * 1024
	uploadCap := 100 * 1024
	port := uint32(1029)
	var seedDuration time.Duration
	var debug bool

	var logLevel slog.LevelVar
	slog.SetDefault(slog.New(tracelog.New(&tracelog.Config{
		Level: &logLevel,
		Sink:  log.Default().Writer(),
	})))

	cl := cmdline.New(true)
	cl.NewGeneralOption(&downloadCap).SetName("down").SetSingle('d').SetUsage("Maximum download rate in bytes/second")
	cl.NewGeneralOption(&uploadCap).SetName("up").SetSingle('u').SetUsage("Maximum upload rate in bytes/second")
	cl.NewGeneralOption(&port).SetName("port").SetSingle('p').SetUsage("Port to use for incoming connections")
	cl.NewGeneralOption(&seedDuration).SetName("seed").SetSingle('s').SetUsage("Seed time")
	cl.NewGeneralOption(&torrent.TrackerUserAgent).SetName("agent").SetSingle('a').SetUsage("User agent to use")
	cl.NewGeneralOption(&debug).SetName("debug").SetUsage("Enable debug logging")

	files := cl.Parse(os.Args[1:])
	if len(files) == 0 {
		fatal.WithErr(errs.New("No file specified"))
	}

	if debug {
		logLevel.Set(slog.LevelDebug)
	}

	f, err := tfs.NewFileFromPath(files[0])
	fatal.IfErr(err)

	var d *dispatcher.Dispatcher
	d, err = dispatcher.NewDispatcher(
		dispatcher.GlobalDownloadCap(downloadCap),
		dispatcher.GlobalUploadCap(uploadCap),
		dispatcher.PortRange(port, port),
	)
	fatal.IfErr(err)

	completeNotifier := make(chan *torrent.Client, 1)
	stoppedNotifier := make(chan *torrent.Client, 1)
	var c *torrent.Client
	c, err = torrent.NewClient(d, f,
		torrent.NotifyWhenDownloadComplete(completeNotifier),
		torrent.NotifyWhenStopped(stoppedNotifier),
		torrent.SeedDuration(seedDuration))
	fatal.IfErr(err)

	t := time.NewTimer(time.Second)
	for {
		select {
		case <-completeNotifier:
			slog.Info("complete")
			extractFiles(c.TorrentFile())
		case <-stoppedNotifier:
			switch c.Status().State {
			case torrent.Errored:
				slog.Error("stopped with error")
			case torrent.Done:
				slog.Info("stopped")
				fatal.IfErr(os.Remove(f.StoragePath()))
			}
			return
		case <-t.C:
			slog.Info(c.Status().String())
			t.Reset(time.Second)
		}
	}
}

func extractFiles(tf *tfs.File) {
	files := tf.EmbeddedFiles()
	dir := "."
	if len(files) > 1 {
		dir = filepath.Join(dir, sanitizePath(tf.Info.Name))
		fatal.IfErr(os.Mkdir(dir, 0o750))
	}
	for _, file := range files {
		path := filepath.Join(dir, sanitizePath(file.Name()))
		if file.IsDir() {
			slog.Info("extract", "dir", path)
			fatal.IfErr(os.Mkdir(path, 0o750))
		} else {
			slog.Info("extract", "file", path)
			r, err := tf.Open(file.Name())
			fatal.IfErr(err)
			var f *os.File
			f, err = os.Create(path)
			fatal.IfErr(err)
			_, err = io.Copy(f, r)
			fatal.IfErr(err)
			fatal.IfErr(f.Close())
			fatal.IfErr(r.Close())
		}
	}
}

func sanitizePath(path string) string {
	var list []string
	path = filepath.Clean(path)
	for {
		var file string
		path, file = filepath.Split(path)
		list = append(list, fs.SanitizeName(file))
		if path == "" || (len(path) == 1 && path[0] == os.PathSeparator) {
			break
		}
	}
	slices.Reverse(list)
	return filepath.Join(list...)
}
