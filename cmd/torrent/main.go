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

	"github.com/richardwilkes/toolbox/v2/errs"
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

// extractTimeout is how long the shutdown registered at exit will then wait for the monitor to finish acting on that
// stop. Extraction copies the whole torrent out of its storage, which runs to minutes for a large one, and cutting
// that short leaves truncated files behind that look complete, so the wait has to be long enough to cover an ordinary
// copy. It is still bounded, since work that has genuinely wedged must not leave someone who pressed Ctrl-C with a
// program that never exits.
const extractTimeout = 5 * time.Minute

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
	monitorDone := make(chan struct{})
	stopAtExit(xos.RunAtExit, c.Stop, monitorDone, extractTimeout)

	t := time.NewTicker(time.Second)
	defer t.Stop()
	m := &monitor{
		status:   c.Status,
		extract:  func() error { return extractFiles(c.TorrentFile()) },
		remove:   func() error { return os.Remove(f.StoragePath()) },
		complete: completeNotifier,
		stopped:  stoppedNotifier,
		tick:     t.C,
	}
	ok := m.run()
	// The exit handling waits on this, so it has to be closed before anything that can end the program is reached:
	// xos.Exit parks any goroutine that calls it while an exit is already being handled, and this one parked with the
	// monitor still marked as running would leave that wait to time out rather than finish.
	close(monitorDone)
	if !ok {
		xos.Exit(1)
	}
	xos.Exit(0)
}

// stopAtExit arranges for the torrent to be stopped when the program exits and for the exit to then wait, for at most
// 'wait', until 'finished' says the monitor is done acting on that stop. Registering the stop is what makes Ctrl-C
// (SIGINT) and SIGTERM shut a download down cleanly, since the registrar installs handlers for both that call
// xos.Exit, which in turn runs what was registered here. Without it, a long-running download is killed where it
// stands: the tracker never receives the stopped announce and goes on handing our dead address to peers, peers are
// dropped mid-write, and the storage file is left open and unflushed rather than being closed by the client's
// shutdown.
//
// Waiting for the monitor is what keeps that exit from cutting it off part way through. The monitor extracts the
// torrent's files on its own goroutine while this runs on the one that took the signal, and the os.Exit that follows
// this doesn't unwind the rest of the program: an interrupt arriving mid-copy would otherwise leave truncated files
// behind that look complete, and one arriving just after would make it a coin toss whether the storage file those
// files came from was removed at all.
//
// The registrar, the stop, the notification and the wait are all supplied by the caller so that what gets registered
// can be tested on its own.
func stopAtExit(register func(func()) int, stop func(time.Duration), finished <-chan struct{}, wait time.Duration) {
	register(func() {
		stop(stopTimeout)
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			slog.Warn("gave up waiting for the torrent's files to be dealt with", "timeout", wait)
		}
	})
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
	extract   func() error
	remove    func() error
	complete  <-chan *torrent.Client
	stopped   <-chan *torrent.Client
	tick      <-chan time.Time
	extracted bool
	failed    bool
}

// run processes notifications until the client stops, returning whether it stopped without error. A torrent that
// stopped in the errored state left its download incomplete, and one whose files couldn't be extracted left nothing
// usable behind; neither is something the caller can react to if the program reports success all the same.
//
// The failures of the side effects are recorded and reported here rather than exited on where they happen, since the
// exit handling runs on the goroutine that took the signal and waits for this one: exiting from here while that wait
// is under way parks this goroutine for good, since xos.Exit never returns to a caller that finds an exit already
// being handled, leaving the wait to run out its timeout instead of ending as soon as the work was done.
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
				// The storage is only given up once its content is safely somewhere else, so an extraction that
				// failed leaves it in place to be unpacked another time rather than throwing the download away.
				if m.extracted {
					if err := m.remove(); err != nil {
						errs.Log(err)
						m.failed = true
					}
				}
			}
			return !m.failed
		case <-m.tick:
			slog.Info(m.status().String())
		}
	}
}

// extractIfNeeded extracts the torrent's files if the download has finished and they haven't been extracted already.
func (m *monitor) extractIfNeeded() {
	if m.extracted || m.failed || m.status().RemainingBytes != 0 {
		return
	}
	if err := m.extract(); err != nil {
		errs.Log(err)
		m.failed = true
		return
	}
	m.extracted = true
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
	return extractFiles(tf)
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

// extractFiles copies the whole of the torrent's content out of its storage and into the local filesystem.
func extractFiles(tf *tfs.File) error {
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
	return fs.WalkDir(tf, ".", func(p string, d fs.DirEntry, err error) error {
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
	})
}

// extractFile copies a single file out of the torrent's storage and into the local filesystem at target. The target is
// created exclusively, so that nothing already sitting there is destroyed to make room for it: a single-file torrent's
// content lands directly in the current directory, where unrelated files of the caller's live, and a torrent names its
// own content, so an extraction is otherwise free to overwrite whatever it likes. That also covers the
// case-insensitive filesystems macOS and Windows use, on which two entries differing only in letter case — which the
// library's duplicate-path validation accepts as distinct — would silently clobber each other.
func extractFile(tf *tfs.File, p, target string) error {
	// The storage file is never a valid target. Creating the file exclusively already refuses it, but the collision is
	// worth naming: what it would destroy is the download itself rather than some unrelated file, and it takes nothing
	// more than a single-file torrent naming its content after the storage file to arrange it, at which point the copy
	// below reads the truncated storage back as EOF and a zero-byte file is reported as successfully extracted.
	if sameFile(target, tf.StoragePath()) {
		return fmt.Errorf("refusing to extract %q over the torrent's own storage file %q", target, tf.StoragePath())
	}
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
	return copyToNewFile(target, r)
}

// copyToNewFile creates target, which must not already exist, and copies everything r has into it.
//
// A copy that fails part way — a full disk, a storage file that can't be read — takes the target with it. What it
// would otherwise leave behind is a truncated file that is indistinguishable from a completed extraction, and since
// the target is created exclusively, every later extraction and every -unpack retry then fails with "file exists" on
// that partial file rather than replacing it. That defeats the retry the monitor deliberately keeps the storage around
// for and leaves whoever asked for the extraction to work out which file to delete by hand. A close that fails is
// treated the same way, since data buffered on our behalf may never have reached the disk.
func copyToNewFile(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, r); err != nil {
		xio.CloseIgnoringErrors(f) // The copy failure is the more meaningful error, so keep it
		removePartialExtraction(target)
		return err
	}
	if err = f.Close(); err != nil {
		removePartialExtraction(target)
		return err
	}
	return nil
}

// removePartialExtraction discards what a failed extraction left at target. A removal that fails itself is logged
// rather than reported, since the failure that led here is the one worth telling the caller about.
func removePartialExtraction(target string) {
	if err := os.Remove(target); err != nil {
		errs.Log(err)
	}
}

// sameFile reports whether both paths name the same existing file. Comparing the paths themselves would miss ones
// spelled differently, ones reached through a link, and ones differing only in letter case on the case-insensitive
// filesystems macOS and Windows use.
func sameFile(a, b string) bool {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
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
			list = append(list, sanitizeComponent(part))
		}
	}
	if len(list) == 0 {
		return sanitizeComponent(cleaned)
	}
	return filepath.Join(list...)
}

// reservedNameEscape is what a name Windows resolves to a device is prefixed with. It follows xfilepath.SanitizeName's
// "@n" scheme and uses a number that scheme doesn't, so it can't collide with anything a sanitized name produces: a
// literal '@' is written as "@3", which leaves "@7" as something only this can put at the front of a name.
const reservedNameEscape = "@7"

// windowsReservedNames are the names Windows resolves to a device rather than to a file, in the upper case form the
// comparison is made against.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeComponent makes one component of a path safe to use as a local file name. xfilepath.SanitizeName covers the
// characters a filesystem refuses, but not the names Windows resolves to a device: CreateFile answers "CON", "NUL",
// "COM1" and their kind with the device itself, in any directory and whatever extension the name carries, and it does
// so before the filesystem is consulted at all. A torrent names its own content, so one naming a file that way has an
// extraction on Windows open the console or the null device — O_EXCL doesn't refuse it, since no file is involved —
// write the content there, and report a successful extraction with nothing on disk to show for it.
//
// The escape is applied on every platform rather than only on Windows, so that the same torrent extracts to the same
// layout wherever it is unpacked and so that the behavior is testable everywhere.
func sanitizeComponent(name string) string {
	sanitized := xfilepath.SanitizeName(name)
	// Windows resolves the device from what precedes the first '.', ignoring trailing spaces and dots, so "COM1.txt"
	// and "CON " name devices just as bare "CON" does.
	device := sanitized
	if i := strings.IndexByte(device, '.'); i != -1 {
		device = device[:i]
	}
	if windowsReservedNames[strings.ToUpper(strings.TrimRight(device, " "))] {
		return reservedNameEscape + sanitized
	}
	return sanitized
}
