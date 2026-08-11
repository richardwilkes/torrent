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
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
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

// stopTimeout is how long the shutdown registered at exit will wait for the torrent to stop. The stopped announce is
// made against a tracker with a 30 second timeout of its own, so anything less would routinely give up on a shutdown
// that was about to finish, while waiting longer would leave someone who pressed Ctrl-C staring at an unresponsive
// program.
const stopTimeout = 30 * time.Second

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
	userAgent := flag.String("agent", torrent.TrackerUserAgent(), "User agent to use")
	unpackOnly := flag.Bool("unpack", false, "Only unpack the torrent")

	var logCfg xslog.Config
	logCfg.Console = true
	logCfg.AddFlags()
	xflag.AddVersionFlags()
	xflag.SetUsage(nil, "", "<torrent file>")
	xflag.Parse()
	torrent.SetTrackerUserAgent(*userAgent)

	torrentPath, err := torrentFilePath(flag.Args())
	if err != nil {
		xos.ExitWithMsg(err.Error())
	}
	portOpt, err := portRangeOption(uint64(*port))
	if err != nil {
		xos.ExitWithMsg(err.Error())
	}

	var f *tfs.File
	f, err = tfs.NewFileFromPath(torrentPath)
	xos.ExitIfErr(err)

	if *unpackOnly {
		slog.Info("unpacking")
		xos.ExitIfErr(unpack(f))
		xos.Exit(0)
	}

	opts := make([]func(*dispatcher.Dispatcher) error, 0, 3)
	opts = append(opts, dispatcher.GlobalDownloadCap(*downloadCap), dispatcher.GlobalUploadCap(*uploadCap))
	if portOpt != nil {
		opts = append(opts, portOpt)
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
	stopAtExit(xos.RunAtExit, c.Stop)

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
	if !m.run() {
		xos.Exit(1)
	}
	xos.Exit(0)
}

// stopAtExit arranges for the torrent to be stopped when the program exits. Registering the stop is what makes Ctrl-C
// (SIGINT) and SIGTERM shut a download down cleanly, since the registrar installs handlers for both that call
// xos.Exit, which in turn runs what was registered here. Without it, a long-running download is killed where it
// stands: the tracker never receives the stopped announce and goes on handing our dead address to peers, peers are
// dropped mid-write, and the storage file is left open and unflushed rather than being closed by the client's
// shutdown. The registrar is supplied by the caller so that what gets registered can be tested on its own.
func stopAtExit(register func(func()) int, stop func(time.Duration)) {
	register(func() { stop(stopTimeout) })
}

// torrentFilePath returns the path of the torrent file to work with. Exactly one is required: only one torrent is
// processed per run, so extra arguments are refused rather than quietly dropped.
func torrentFilePath(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", errors.New("no torrent file specified")
	case 1:
		return args[0], nil
	default:
		return "", errors.New("only one torrent file may be specified")
	}
}

// portRangeOption turns the requested port into a dispatcher option, returning a nil option when the system should
// choose the port itself. The range has to be checked here rather than left to dispatcher.PortRange, since the flag
// accepts any unsigned value and narrowing it to the uint32 that PortRange takes would wrap a larger one into the
// valid range and quietly listen on the wrong port.
func portRangeOption(port uint64) (func(*dispatcher.Dispatcher) error, error) {
	if port == 0 {
		return nil, nil
	}
	if port > math.MaxUint16 {
		return nil, fmt.Errorf("port must be in the range 1 to %d", math.MaxUint16)
	}
	return dispatcher.PortRange(uint32(port), uint32(port)), nil
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

// run processes notifications until the client stops, returning whether it stopped without error. A torrent that
// stopped in the errored state left its download incomplete, which the caller has no way to react to if the program
// reports success all the same.
func (m *monitor) run() bool {
	for {
		select {
		case <-m.complete:
			slog.Info("complete")
			m.extractIfNeeded()
		case <-m.stopped:
			switch m.status().State {
			case torrent.Errored:
				slog.Error("stopped with error")
				return false
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
			return true
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

// unpack extracts the torrent's files from storage that has already been downloaded, refusing to do so unless that
// storage actually holds the whole torrent. The client preallocates the storage file at its full length as soon as a
// download starts, so an unpack of an interrupted download would otherwise write out full-size files of whatever the
// storage happened to hold — zeros, for the most part — and report success, which is indistinguishable from the real
// thing until whoever asked for it tries to use the result. The notification path this shares its extraction with
// already refuses to run until nothing is left to download.
func unpack(tf *tfs.File) error {
	if err := verifyStorageIsComplete(tf); err != nil {
		return err
	}
	extractFiles(tf)
	return nil
}

// verifyStorageIsComplete returns an error unless every piece of the torrent is present in its storage and hashes to
// what the torrent says it should.
func verifyStorageIsComplete(tf *tfs.File) error {
	f, err := os.Open(tf.StoragePath())
	if err != nil {
		return err
	}
	defer xio.CloseIgnoringErrors(f)
	count := tf.PieceCount()
	buffer := make([]byte, tf.Info.PieceLength)
	for i := range count {
		length := int(tf.LengthOf(i))
		var n int
		if n, err = f.ReadAt(buffer[:length], tf.OffsetOf(i)); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n != length || !tf.Validate(i, buffer[:n]) {
			return fmt.Errorf("%s does not hold the complete torrent: piece %d of %d is missing or damaged",
				tf.StoragePath(), i+1, count)
		}
	}
	return nil
}

func extractFiles(tf *tfs.File) {
	dir := "."
	// By convention, a multi-file torrent's content goes into a directory named for the torrent while a single-file
	// torrent's content goes directly into the current directory. Which form a torrent uses is determined by whether
	// it carries a file list, not by how many files that list holds: a multi-file torrent with exactly one entry still
	// gets the wrapping directory.
	if len(tf.Info.Files) != 0 {
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

// sanitizePath makes each component of a slash-separated virtual path safe to use as a local filesystem path. The
// result is never empty: a path whose components all fall away, such as "." or "/", is sanitized as a single name
// instead, the same way ".." already is. Joining an empty result onto a directory silently drops the component, and
// the torrent validation accepts either of those as a torrent's name, so a multi-file torrent carrying one would be
// extracted straight into the current directory rather than into the directory named for it, on top of whatever is
// already there.
func sanitizePath(p string) string {
	cleaned := path.Clean(p)
	parts := strings.Split(cleaned, "/")
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." {
			list = append(list, xfilepath.SanitizeName(part))
		}
	}
	if len(list) == 0 {
		return xfilepath.SanitizeName(cleaned)
	}
	return filepath.Join(list...)
}
