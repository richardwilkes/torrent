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
	"bufio"
	"crypto/sha1" //nolint:gosec // The spec requires sha1
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xos"
	"github.com/richardwilkes/torrent"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/zeebo/bencode"
)

const (
	// notifyWait is how long a test will wait for a notification to be acted on.
	notifyWait = 5 * time.Second

	extractAction = "extract"
	removeAction  = "remove"

	lengthKey = "length"
	pathKey   = "path"
	filesKey  = "files"
	piecesKey = "pieces"
	fileB     = "b.txt"

	// torrentName is the name carried by the torrents the extraction tests build, and therefore the directory a
	// multi-file one is expected to land in.
	torrentName = "example"

	// sampleContent is the storage content of the torrents the extraction tests build. Its length has to agree with
	// the piece length and piece count newTorrentFile supplies.
	sampleContent = "0123456789abcdefghij"

	// samplePieceLength is the piece length newTorrentFile builds its torrents with.
	samplePieceLength = 16

	// exitHelperEnv marks the child process the exit handling test starts, and exitHelperName is the test that child
	// runs. The lines the child reports are how the parent follows what its exit handling did.
	exitHelperEnv  = "TORRENT_TEST_EXIT_HELPER"
	exitHelperName = "TestStopAtExitHelper"
	readyLine      = "registered"
	stoppedLine    = "stopped"
	extractedLine  = "extracted"

	windowsOS = "windows"
)

// errExtractFailed and errRemoveFailed stand in for the extraction and removal of a torrent's files going wrong.
var (
	errExtractFailed = errors.New("unable to extract")
	errRemoveFailed  = errors.New("unable to remove")
)

// TestMonitorStop verifies what is done when the client stops. The completion and stopped notifications are sent in
// quick succession when seeding ends, so a select with both of them ready picks between them at random and the stopped
// one may even arrive first, but the files must always be extracted before the storage they came from is removed.
func TestMonitorStop(t *testing.T) {
	for _, one := range []struct {
		name             string
		extractErr       error
		removeErr        error
		want             []string
		remainingBytes   int64
		state            torrent.State
		completePending  bool
		alreadyExtracted bool
		wantOK           bool
	}{
		{
			name:            "the stopped notification races the completion notification",
			state:           torrent.Done,
			completePending: true,
			want:            []string{extractAction, removeAction},
			wantOK:          true,
		},
		{
			name:   "the completion notification hasn't been sent yet",
			state:  torrent.Done,
			want:   []string{extractAction, removeAction},
			wantOK: true,
		},
		{
			name:             "the completion notification already extracted the files",
			state:            torrent.Done,
			alreadyExtracted: true,
			want:             []string{removeAction},
			wantOK:           true,
		},
		{
			name:           "stopped before the download finished",
			state:          torrent.Done,
			remainingBytes: 1024,
			want:           nil,
			wantOK:         true,
		},
		{
			name:   "stopped with an error",
			state:  torrent.Errored,
			want:   nil,
			wantOK: false,
		},
		{
			// The storage is all that is left of the download when the extraction of it fails, so it has to stay put
			// for another attempt rather than being removed along with everything else
			name:       "the files could not be extracted",
			state:      torrent.Done,
			extractErr: errExtractFailed,
			want:       []string{extractAction},
			wantOK:     false,
		},
		{
			name:      "the storage could not be removed",
			state:     torrent.Done,
			removeErr: errRemoveFailed,
			want:      []string{extractAction, removeAction},
			wantOK:    false,
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			complete := make(chan *torrent.Client, 1)
			stopped := make(chan *torrent.Client, 1)
			if one.completePending {
				complete <- nil
			}
			stopped <- nil
			var actions []string
			m := &monitor{
				status: func() *torrent.Status {
					return &torrent.Status{State: one.state, RemainingBytes: one.remainingBytes}
				},
				extract: func() error {
					actions = append(actions, extractAction)
					return one.extractErr
				},
				remove: func() error {
					actions = append(actions, removeAction)
					return one.removeErr
				},
				complete:  complete,
				stopped:   stopped,
				extracted: one.alreadyExtracted,
			}
			c.Equal(one.wantOK, m.run(), "a torrent that stopped with an error, or that left nothing usable behind, "+
				"must be reported as such, so that the program can exit with a status that says the download failed")
			c.Equal(one.want, actions)
		})
	}
}

// TestMonitorCompleteThenStop verifies the ordinary path, where the completion notification is acted on by itself and
// the stop that follows doesn't extract the files a second time.
func TestMonitorCompleteThenStop(t *testing.T) {
	c := check.New(t)
	complete := make(chan *torrent.Client, 1)
	stopped := make(chan *torrent.Client, 1)
	complete <- nil
	extracted := make(chan struct{}, 1)
	var actions []string
	m := &monitor{
		status: func() *torrent.Status { return &torrent.Status{State: torrent.Done} },
		extract: func() error {
			actions = append(actions, extractAction)
			extracted <- struct{}{}
			return nil
		},
		remove:   func() error { actions = append(actions, removeAction); return nil },
		complete: complete,
		stopped:  stopped,
	}

	// Hold the stop back until the completion has been acted on, so that the two can't be selected between. The stop
	// is sent either way, since run would otherwise never return, but whether the completion was what extracted the
	// files is what decides the test: the stop path extracts them too, so without this a completion path that does
	// nothing at all still leaves the same actions behind.
	extractedOnCompletion := make(chan bool, 1)
	go func() {
		select {
		case <-extracted:
			extractedOnCompletion <- true
		case <-time.After(notifyWait):
			extractedOnCompletion <- false
		}
		stopped <- nil
	}()
	c.True(m.run(), "a torrent that stopped without an error must not be reported as a failure")
	c.True(<-extractedOnCompletion, "the completion notification must extract the files by itself")
	c.Equal([]string{extractAction, removeAction}, actions)
}

// TestStopAtExit verifies that the torrent is stopped when the program exits, which is what a Ctrl-C or a SIGTERM
// turns into, and that the exit then waits for the monitor to finish acting on that stop. Without the stop, the
// process dies where it stands: the tracker never receives the stopped announce, peers are dropped mid-write and the
// client's shutdown never runs. Without the wait, the os.Exit that follows kills the extraction the monitor performs
// part way through, leaving truncated files behind that look complete.
func TestStopAtExit(t *testing.T) {
	c := check.New(t)
	registered := registerStopAtExit(t, func(timeout time.Duration) { c.Equal(stopTimeout, timeout) },
		make(chan struct{}), notifyWait)

	returned := make(chan struct{})
	go func() {
		registered()
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("the exit did not wait for the monitor to finish with the torrent's files")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestStopAtExitResumesOnceTheMonitorIsDone verifies that the exit picks back up as soon as the monitor reports that
// it has finished, rather than sitting out the whole of the time it was allowed to wait.
func TestStopAtExitResumesOnceTheMonitorIsDone(t *testing.T) {
	c := check.New(t)
	stopped := make(chan time.Duration, 1)
	finished := make(chan struct{})
	registered := registerStopAtExit(t, func(timeout time.Duration) { stopped <- timeout }, finished, time.Hour)

	returned := make(chan struct{})
	go func() {
		registered()
		close(returned)
	}()
	select {
	case timeout := <-stopped:
		c.Equal(stopTimeout, timeout)
	case <-time.After(notifyWait):
		t.Fatal("the torrent was not stopped")
	}
	close(finished)
	select {
	case <-returned:
	case <-time.After(notifyWait):
		t.Fatal("the exit did not resume once the monitor had finished with the torrent's files")
	}
}

// TestStopAtExitGivesUpWaitingForTheMonitor verifies that the wait for the monitor is bounded. Extraction that has
// genuinely wedged must not leave someone who pressed Ctrl-C with a program that never exits.
func TestStopAtExitGivesUpWaitingForTheMonitor(t *testing.T) {
	registered := registerStopAtExit(t, func(_ time.Duration) {}, make(chan struct{}), time.Millisecond)

	returned := make(chan struct{})
	go func() {
		registered()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(notifyWait):
		t.Fatal("the exit never gave up waiting for the monitor")
	}
}

// registerStopAtExit registers the exit handling with a stand-in registrar and returns what was registered with it.
func registerStopAtExit(t *testing.T, stop func(time.Duration), finished <-chan struct{}, wait time.Duration) func() {
	t.Helper()
	var registered func()
	stopAtExit(func(f func()) int {
		registered = f
		return 1
	}, stop, finished, wait)
	if registered == nil {
		// Fatal, since calling what wasn't registered would panic and take the whole test binary down with it
		t.Fatal("nothing was registered to run when the program exits")
	}
	return registered
}

// TestStopAtExitRunsOnInterrupt verifies the whole chain a Ctrl-C travels: the signal handlers the registrar installs,
// the exit handling those hand off to, and the stop that was registered with it. Observing that means letting the exit
// handling run to completion, which ends the process it happens in, so the test binary re-runs itself as the child
// process below and interrupts it.
func TestStopAtExitRunsOnInterrupt(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("interrupts cannot be sent to another process on this platform")
	}
	c := check.New(t)
	// The checks that everything below rests on are fatal rather than the package's ordinary non-fatal ones: a test
	// that carried on from any of them would reach the deferred kill with a nil cmd.Process and panic the whole test
	// binary instead of reporting a clean failure
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^"+exitHelperName+"$", "-test.timeout="+(2*notifyWait).String())
	cmd.Env = append(os.Environ(), exitHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	defer func() {
		_ = cmd.Process.Kill() //nolint:errcheck // Nothing can be done if the child is already gone
		_ = cmd.Wait()         //nolint:errcheck // The exit status is checked below when it is reached normally
	}()

	// Wait until the child has registered its stop, since a signal sent before that would kill it outright
	waitForLine(t, lines, readyLine)

	c.NoError(cmd.Process.Signal(os.Interrupt))
	waitForLine(t, lines, stoppedLine)
	// The exit handling has to hold the process open until the stand-in for the monitor has reported that it finished
	// with the torrent's files, since the os.Exit that follows would otherwise cut an extraction off mid-copy
	waitForLine(t, lines, extractedLine)
}

// waitForLine fails the test if the child process doesn't report the expected line promptly.
func waitForLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	deadline := time.After(notifyWait)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the child process ended without reporting %q", want)
			}
			// The backspaces are trimmed along with the whitespace, since the interrupt handling emits a pair of them
			// to erase the "^C" a terminal echoes, and they land in front of whatever is reported next
			if strings.Trim(line, " \t\r\n\b") == want {
				return
			}
		case <-deadline:
			t.Fatalf("the child process did not report %q", want)
		}
	}
}

// TestStopAtExitHelper is the child process of TestStopAtExitRunsOnInterrupt rather than a test in its own right. It
// registers the stop the same way the program does and then waits to be interrupted.
func TestStopAtExitHelper(t *testing.T) {
	if os.Getenv(exitHelperEnv) != "1" {
		t.Skip("only runs as the child process of TestStopAtExitRunsOnInterrupt")
	}
	// Stands in for the monitor, which only has the torrent's files to deal with once the stop has been made, and
	// takes long enough over them that an exit which didn't wait would be gone before it had finished
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		<-stopped
		time.Sleep(100 * time.Millisecond)
		fmt.Println(extractedLine)
		close(finished)
	}()
	stopAtExit(xos.RunAtExit, func(timeout time.Duration) {
		if timeout == stopTimeout {
			fmt.Println(stoppedLine)
		}
		close(stopped)
	}, finished, notifyWait)
	fmt.Println(readyLine)
	time.Sleep(2 * notifyWait) // The interrupt the parent is about to send is what ends this
}

// newTorrentFile builds a torrent whose storage lives in the current directory and holds sampleContent. Only the part
// of the info dictionary that describes the content's layout is supplied by the caller, since that is what the
// extraction tests vary: either a length, for the single-file form, or a file list, for the multi-file one.
func newTorrentFile(t *testing.T, name string, layout map[string]any) *tfs.File {
	t.Helper()
	info := map[string]any{
		"name":         name,
		"piece length": int64(samplePieceLength),
		piecesKey:      make([]byte, 40),
	}
	maps.Copy(info, layout)
	// The checks here are fatal rather than the package's ordinary non-fatal ones: every caller goes straight on to
	// use what is returned, so a test that carried on from a failure would dereference a nil file and panic the whole
	// test binary instead of reporting a clean failure
	data, err := bencode.EncodeBytes(map[string]any{"info": info})
	if err != nil {
		t.Fatal(err)
	}
	f, err := tfs.NewFileFromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.StoragePath(), []byte(sampleContent), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestExtractFiles verifies that a torrent's files land on disk with their full virtual paths intact. The file info
// the torrent carries holds only base names, so the paths have to come from walking the tree.
func TestExtractFiles(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())

	tf := newTorrentFile(t, torrentName, map[string]any{
		filesKey: []any{
			map[string]any{lengthKey: int64(12), pathKey: []any{"sub", "a.txt"}},
			map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
		},
	})
	c.NoError(extractFiles(tf))

	data, err := os.ReadFile(filepath.Join(torrentName, "sub", "a.txt"))
	c.NoError(err)
	c.Equal("0123456789ab", string(data))
	data, err = os.ReadFile(filepath.Join(torrentName, fileB))
	c.NoError(err)
	c.Equal("cdefghij", string(data))
}

// TestExtractFilesSingle verifies that a single-file torrent lands in the current directory rather than in a
// wrapping directory of its own.
func TestExtractFilesSingle(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())

	tf := newTorrentFile(t, "example.bin", map[string]any{lengthKey: int64(20)})
	c.NoError(extractFiles(tf))

	data, err := os.ReadFile("example.bin")
	c.NoError(err)
	c.Equal(sampleContent, string(data))
}

// TestExtractFilesMultiFileWithOneEntry verifies that a torrent using the multi-file form still lands in a directory
// named for the torrent when its file list holds exactly one entry. Which form the torrent uses is what decides this,
// not how many files it happens to carry.
func TestExtractFilesMultiFileWithOneEntry(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())

	tf := newTorrentFile(t, torrentName, map[string]any{
		filesKey: []any{map[string]any{lengthKey: int64(20), pathKey: []any{fileB}}},
	})
	c.NoError(extractFiles(tf))

	data, err := os.ReadFile(filepath.Join(torrentName, fileB))
	c.NoError(err)
	c.Equal(sampleContent, string(data))
	_, err = os.Stat(fileB)
	c.HasError(err, "the file must not be extracted into the current directory")
}

// TestExtractFilesWithANameThatSanitizesAway verifies that a multi-file torrent whose name is made up entirely of
// separators and dots still gets a wrapping directory of its own. The torrent validation accepts such a name, and one
// that sanitized away to nothing would leave the content extracted straight into the current directory, on top of any
// file already sitting there under the same name.
func TestExtractFilesWithANameThatSanitizesAway(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())
	const existing = "do not overwrite me"
	c.NoError(os.WriteFile(fileB, []byte(existing), 0o600))

	tf := newTorrentFile(t, ".", map[string]any{
		filesKey: []any{map[string]any{lengthKey: int64(20), pathKey: []any{fileB}}},
	})
	c.NoError(extractFiles(tf))

	data, err := os.ReadFile(filepath.Join(sanitizePath(tf.Info.Name), fileB))
	c.NoError(err)
	c.Equal(sampleContent, string(data))
	data, err = os.ReadFile(fileB)
	c.NoError(err)
	c.Equal(existing, string(data), "the file already in the current directory must not be overwritten")
}

// TestExtractFilesRefusesToExtractOverTheStorageFile verifies that a torrent naming its content after the storage file
// it was downloaded into is refused rather than extracted. A single-file torrent's content lands directly in the
// current directory, which is where its storage file lives too, and the torrent chooses the name: creating the target
// would truncate the very file the extraction is reading out of, so the copy would see EOF, report a zero-byte file as
// successfully extracted, and the download would be gone.
func TestExtractFilesRefusesToExtractOverTheStorageFile(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())

	tf := newTorrentFile(t, torrentName+tfs.DownloadExt, map[string]any{lengthKey: int64(len(sampleContent))})
	c.Equal(torrentName+tfs.DownloadExt, tf.StoragePath(), "the torrent must name its content after its storage file")
	c.HasError(extractFiles(tf), "extracting a torrent over its own storage file must be refused")

	data, err := os.ReadFile(tf.StoragePath())
	c.NoError(err)
	c.Equal(sampleContent, string(data), "the storage file must still hold everything that was downloaded")
}

// TestExtractFilesDoesNotOverwriteAnExistingFile verifies that a file already sitting at the target is left alone
// rather than being truncated to make room for the extraction. A single-file torrent's content lands directly in the
// current directory, alongside whatever unrelated files of the caller's are there, and the torrent chooses the name.
func TestExtractFilesDoesNotOverwriteAnExistingFile(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())
	const existing = "do not overwrite me"
	const target = torrentName + ".bin"
	c.NoError(os.WriteFile(target, []byte(existing), 0o600))

	tf := newTorrentFile(t, target, map[string]any{lengthKey: int64(len(sampleContent))})
	c.HasError(extractFiles(tf), "a file already at the target must not be overwritten")

	data, err := os.ReadFile(target)
	c.NoError(err)
	c.Equal(existing, string(data), "the file already in the current directory must not be overwritten")
}

// TestUnpackRefusesStorageThatIsNotComplete verifies that unpacking a torrent whose download never finished writes
// nothing at all. The storage file is preallocated at its full length the moment a download starts, so extracting from
// it without checking produces full-size files of whatever it happens to hold — zeros, for the most part — and
// reports success, which is indistinguishable from the real thing until someone tries to use the result.
func TestUnpackRefusesStorageThatIsNotComplete(t *testing.T) {
	for _, one := range []struct {
		name    string
		content string
	}{
		{name: "storage holding the whole torrent", content: sampleContent},
		{name: "storage that was never filled in", content: strings.Repeat("\x00", len(sampleContent))},
		{name: "storage holding all but the last piece", content: sampleContent[:samplePieceLength] + "\x00\x00\x00\x00"},
		{name: "storage shorter than the torrent", content: sampleContent[:8]},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			t.Chdir(t.TempDir())
			tf := newTorrentFile(t, torrentName, map[string]any{
				filesKey:  []any{map[string]any{lengthKey: int64(len(sampleContent)), pathKey: []any{fileB}}},
				piecesKey: samplePieceHashes(),
			})
			c.NoError(os.WriteFile(tf.StoragePath(), []byte(one.content), 0o600))

			err := unpack(tf)
			if one.content == sampleContent {
				c.NoError(err)
				data, rerr := os.ReadFile(filepath.Join(torrentName, fileB))
				c.NoError(rerr)
				c.Equal(sampleContent, string(data))
				return
			}
			c.HasError(err, "an incomplete torrent must not be unpacked")
			_, err = os.Stat(filepath.Join(torrentName, fileB))
			c.HasError(err, "nothing may be extracted from storage that isn't complete")
		})
	}
}

// samplePieceHashes returns the piece hashes for a torrent whose storage holds sampleContent, at the piece length
// newTorrentFile builds its torrents with.
func samplePieceHashes() []byte {
	hashes := make([]byte, 0, sha1.Size*((len(sampleContent)+samplePieceLength-1)/samplePieceLength))
	for i := 0; i < len(sampleContent); i += samplePieceLength {
		sum := sha1.Sum([]byte(sampleContent[i:min(i+samplePieceLength, len(sampleContent))])) //nolint:gosec // The spec requires sha1
		hashes = append(hashes, sum[:]...)
	}
	return hashes
}

// TestTorrentFilePath verifies that exactly one torrent file must be named, since only one is processed per run and
// silently dropping the rest would leave the caller thinking they had all been handled.
func TestTorrentFilePath(t *testing.T) {
	c := check.New(t)
	for _, one := range []struct {
		name    string
		want    string
		args    []string
		wantErr bool
	}{
		{name: "no torrent file", wantErr: true},
		{name: "one torrent file", args: []string{"a.torrent"}, want: "a.torrent"},
		{name: "more than one torrent file", args: []string{"first.torrent", "second.torrent"}, wantErr: true},
	} {
		p, err := torrentFilePath(one.args)
		if one.wantErr {
			c.HasError(err, one.name)
			continue
		}
		c.NoError(err, one.name)
		c.Equal(one.want, p, one.name)
	}
}

// TestPortRangeOption verifies that the requested port is range-checked before it is narrowed to the 32 bits the
// dispatcher option takes, so a value too large to be a port can't wrap into one the dispatcher accepts.
func TestPortRangeOption(t *testing.T) {
	c := check.New(t)
	for _, one := range []struct {
		name    string
		port    uint64
		wantOpt bool
		wantErr bool
	}{
		{name: "zero leaves the choice to the system"},
		{name: "the lowest valid port", port: 1, wantOpt: true},
		{name: "the highest valid port", port: 65535, wantOpt: true},
		{name: "one past the highest valid port", port: 65536, wantErr: true},
		{name: "a value that would truncate to zero", port: 1 << 32, wantErr: true},
		{name: "a value that would truncate to a valid port", port: 1<<32 + 1, wantErr: true},
	} {
		opt, err := portRangeOption(one.port)
		if one.wantErr {
			c.HasError(err, one.name)
			c.Nil(opt, one.name)
			continue
		}
		c.NoError(err, one.name)
		if !one.wantOpt {
			c.Nil(opt, one.name)
			continue
		}
		c.NotNil(opt, one.name)
		c.NoError(opt(&dispatcher.Dispatcher{}), one.name)
	}
}

func TestSanitizePath(t *testing.T) {
	c := check.New(t)
	for _, one := range []struct {
		in   string
		want []string
	}{
		{in: "a/" + fileB, want: []string{"a", fileB}},
		{in: "./a/../" + fileB, want: []string{fileB}},
		{in: "a//" + fileB, want: []string{"a", fileB}},
		{in: "a/:" + fileB, want: []string{"a", "@6" + fileB}},
		// Paths whose components all fall away are sanitized as a single name, the same way ".." already is, since a
		// caller joining an empty result onto a directory would silently be left with the directory itself
		{in: "..", want: []string{"@2"}},
		{in: ".", want: []string{"@1"}},
		{in: "", want: []string{"@1"}},
		{in: "./.", want: []string{"@1"}},
		{in: "/", want: []string{"@4"}},
		{in: "//", want: []string{"@4"}},
	} {
		actual := sanitizePath(one.in)
		c.Equal(filepath.Join(one.want...), actual, one.in)
		c.NotEqual("", actual, "a sanitized path may never be empty: %q", one.in)
	}
}
