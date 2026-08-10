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
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/richardwilkes/toolbox/v2/xfilepath"
	"github.com/richardwilkes/toolbox/v2/xflag"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/toolbox/v2/xos"
	"github.com/richardwilkes/toolbox/v2/xslog"
	"github.com/richardwilkes/torrent"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
)

func main() {
	xos.AppName = "Simple Torrent"
	xos.AppCmdName = "torrent"
	xos.AppVersion = "1.5.1"
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

	t := time.NewTicker(time.Second)
	defer t.Stop()
	m := &monitor{
		status:   c.Status,
		extract:  func() { extractFiles(c.TorrentFile()) },
		remove:   func() { xos.ExitIfErr(os.Remove(f.StoragePath())) },
		complete: completeNotifier,
		stopped:  stoppedNotifier,
		tick:     t.C,
	}
	m.run()
	xos.Exit(0)
}

// monitor acts on a client's notifications until it stops. The side effects are supplied by the caller, rather than
// reached through the client, so that the handling of the notifications can be tested on its own.
type monitor struct {
	status    func() *torrent.Status
	extract   func()
	remove    func()
	complete  <-chan *torrent.Client
	stopped   <-chan *torrent.Client
	tick      <-chan time.Time
	extracted bool
}

// run processes notifications until the client stops.
func (m *monitor) run() {
	for {
		select {
		case <-m.complete:
			slog.Info("complete")
			m.extractIfNeeded()
		case <-m.stopped:
			switch m.status().State {
			case torrent.Errored:
				slog.Error("stopped with error")
			case torrent.Done:
				slog.Info("stopped")
				// The client sends the completion and stopped notifications in quick succession when seeding ends,
				// and it may not even have sent the completion one yet, so the files may still be waiting to be
				// extracted. They have to be, since the storage they came from is about to be removed.
				m.extractIfNeeded()
				if m.extracted {
					m.remove()
				}
			}
			return
		case <-m.tick:
			slog.Info(m.status().String())
		}
	}
}

// extractIfNeeded extracts the torrent's files if the download has finished and they haven't been extracted already.
func (m *monitor) extractIfNeeded() {
	if !m.extracted && m.status().RemainingBytes == 0 {
		m.extract()
		m.extracted = true
	}
}

func extractFiles(tf *tfs.File) {
	dir := "."
	if len(tf.EmbeddedFiles()) > 1 {
		dir = filepath.Join(dir, sanitizePath(tf.Info.Name))
	}
	// The torrent's info only carries the base name of each entry, so walk the tree to recover the paths that Open
	// expects and that determine where each file lands on disk.
	xos.ExitIfErr(fs.WalkDir(tf, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		target := filepath.Join(dir, sanitizePath(p))
		if d.IsDir() {
			slog.Info("extract", "dir", target)
			return os.MkdirAll(target, 0o750)
		}
		slog.Info("extract", "file", target)
		return extractFile(tf, p, target)
	}))
}

// extractFile copies a single file out of the torrent's storage and into the local filesystem at target.
func extractFile(tf *tfs.File, p, target string) error {
	r, err := tf.Open(p)
	if err != nil {
		return err
	}
	defer xio.CloseIgnoringErrors(r)
	if d, _ := filepath.Split(target); d != "" {
		if err = os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, r); err != nil {
		xio.CloseIgnoringErrors(f) // The copy failure is the more meaningful error, so keep it
		return err
	}
	return f.Close()
}

// sanitizePath makes each component of a slash-separated virtual path safe to use as a local filesystem path.
func sanitizePath(p string) string {
	parts := strings.Split(path.Clean(p), "/")
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." {
			list = append(list, xfilepath.SanitizeName(part))
		}
	}
	return filepath.Join(list...)
}
