// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package main

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/richardwilkes/toolbox/v2/xfilepath"
	"github.com/richardwilkes/toolbox/v2/xflag"
	"github.com/richardwilkes/toolbox/v2/xos"
	"github.com/richardwilkes/toolbox/v2/xslog"
	"github.com/richardwilkes/torrent"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
)

func main() {
	xos.AppName = "Simple Torrent"
	xos.AppCmdName = "torrent"
	xos.AppVersion = "1.5.0"
	xos.License = "Mozilla Public License, version 2.0"
	xos.CopyrightStartYear = "2017"
	xos.CopyrightHolder = "Richard A. Wilkes"
	xos.AppIdentifier = "com.trollworks.torrent"

	downloadCap := flag.Int("down", 300*1024*1024, "Maximum download rate in `bytes`/second")
	uploadCap := flag.Int("up", 100*1024, "Maximum upload rate in `bytes`/second")
	port := flag.Uint("port", 0, "Port to use for incoming connections (use 0 for random)")
	seedDuration := flag.Duration("seed", 0, "Seed time")
	flag.StringVar(&torrent.TrackerUserAgent, "agent", torrent.TrackerUserAgent, "User agent to use")
	unpackOnly := flag.Bool("unpack", false, "Only unpack the torrent")

	var logCfg xslog.Config
	logCfg.Console = true
	logCfg.AddFlags()
	xflag.AddVersionFlags()
	xflag.SetUsage(nil, "", "")
	xflag.Parse()
	files := flag.Args()
	if len(files) == 0 {
		xos.ExitWithMsg("No file specified")
	}

	f, err := tfs.NewFileFromPath(files[0])
	xos.ExitIfErr(err)

	if *unpackOnly {
		slog.Info("unpacking")
		extractFiles(f)
		xos.Exit(0)
	}

	opts := make([]func(*dispatcher.Dispatcher) error, 0, 3)
	opts = append(opts, dispatcher.GlobalDownloadCap(*downloadCap), dispatcher.GlobalUploadCap(*uploadCap))
	if *port != 0 {
		opts = append(opts, dispatcher.PortRange(uint32(*port), uint32(*port)))
	}

	var d *dispatcher.Dispatcher
	d, err = dispatcher.NewDispatcher(opts...)
	xos.ExitIfErr(err)

	completeNotifier := make(chan *torrent.Client, 1)
	stoppedNotifier := make(chan *torrent.Client, 1)
	var c *torrent.Client
	c, err = torrent.NewClient(d, f,
		torrent.NotifyWhenDownloadComplete(completeNotifier),
		torrent.NotifyWhenStopped(stoppedNotifier),
		torrent.SeedDuration(*seedDuration))
	xos.ExitIfErr(err)

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
				xos.ExitIfErr(os.Remove(f.StoragePath()))
			}
			xos.Exit(0)
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
	}
	for _, file := range files {
		path := filepath.Join(dir, sanitizePath(file.Name()))
		if file.IsDir() {
			slog.Info("extract", "dir", path)
			xos.ExitIfErr(os.Mkdir(path, 0o750))
		} else {
			slog.Info("extract", "file", path)
			r, err := tf.Open(file.Name())
			xos.ExitIfErr(err)
			d, _ := filepath.Split(path)
			xos.ExitIfErr(os.MkdirAll(d, 0o750))
			var f *os.File
			f, err = os.Create(path)
			xos.ExitIfErr(err)
			_, err = io.Copy(f, r)
			xos.ExitIfErr(err)
			xos.ExitIfErr(f.Close())
			xos.ExitIfErr(r.Close())
		}
	}
}

func sanitizePath(path string) string {
	parts := strings.Split(filepath.Clean(path), string(os.PathSeparator))
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			list = append(list, xfilepath.SanitizeName(part))
		}
	}
	return filepath.Join(list...)
}
