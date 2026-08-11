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
	fileB     = "b.txt"

	// torrentName is the name carried by the torrents the extraction tests build, and therefore the directory a
	// multi-file one is expected to land in.
	torrentName = "example"

	// sampleContent is the storage content of the torrents the extraction tests build. Its length has to agree with
	// the piece length and piece count newTorrentFile supplies.
	sampleContent = "0123456789abcdefghij"

	// exitHelperEnv marks the child process the exit handling test starts, and exitHelperName is the test that child
	// runs. The lines the child reports are how the parent follows what its exit handling did.
	exitHelperEnv  = "TORRENT_TEST_EXIT_HELPER"
	exitHelperName = "TestStopAtExitHelper"
	readyLine      = "registered"
	stoppedLine    = "stopped"

	windowsOS = "windows"
)

// TestMonitorStop verifies what is done when the client stops. The completion and stopped notifications are sent in
// quick succession when seeding ends, so a select with both of them ready picks between them at random and the stopped
// one may even arrive first, but the files must always be extracted before the storage they came from is removed.
func TestMonitorStop(t *testing.T) {
	for _, one := range []struct {
		name             string
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
				extract:   func() { actions = append(actions, extractAction) },
				remove:    func() { actions = append(actions, removeAction) },
				complete:  complete,
				stopped:   stopped,
				extracted: one.alreadyExtracted,
			}
			c.Equal(one.wantOK, m.run(), "a torrent that stopped with an error must be reported as such, so that "+
				"the program can exit with a status that says the download failed")
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
		extract: func() {
			actions = append(actions, extractAction)
			extracted <- struct{}{}
		},
		remove:   func() { actions = append(actions, removeAction) },
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
// turns into. Without it, the process dies where it stands: the tracker never receives the stopped announce, peers
// are dropped mid-write and the client's shutdown never runs.
func TestStopAtExit(t *testing.T) {
	c := check.New(t)
	var registered func()
	stopped := make(chan time.Duration, 1)
	stopAtExit(func(f func()) int {
		registered = f
		return 1
	}, func(timeout time.Duration) { stopped <- timeout })
	if registered == nil {
		// Fatal, since calling what wasn't registered would panic and take the whole test binary down with it
		t.Fatal("nothing was registered to run when the program exits")
	}

	registered()
	select {
	case timeout := <-stopped:
		c.Equal(stopTimeout, timeout)
	default:
		t.Fatal("the torrent was not stopped")
	}
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
	self, err := os.Executable()
	c.NoError(err)
	cmd := exec.Command(self, "-test.run=^"+exitHelperName+"$", "-test.timeout="+notifyWait.String())
	cmd.Env = append(os.Environ(), exitHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	c.NoError(err)
	c.NoError(cmd.Start())
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
	stopAtExit(xos.RunAtExit, func(timeout time.Duration) {
		if timeout != stopTimeout {
			return
		}
		fmt.Println(stoppedLine)
	})
	fmt.Println(readyLine)
	time.Sleep(notifyWait) // The interrupt the parent is about to send is what ends this
}

// newTorrentFile builds a torrent whose storage lives in the current directory and holds sampleContent. Only the part
// of the info dictionary that describes the content's layout is supplied by the caller, since that is what the
// extraction tests vary: either a length, for the single-file form, or a file list, for the multi-file one.
func newTorrentFile(t *testing.T, name string, layout map[string]any) *tfs.File {
	t.Helper()
	c := check.New(t)
	info := map[string]any{
		"name":         name,
		"piece length": int64(16),
		"pieces":       make([]byte, 40),
	}
	maps.Copy(info, layout)
	data, err := bencode.EncodeBytes(map[string]any{"info": info})
	c.NoError(err)
	f, err := tfs.NewFileFromBytes(data)
	c.NoError(err)
	c.NoError(os.WriteFile(f.StoragePath(), []byte(sampleContent), 0o600))
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
	extractFiles(tf)

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
	extractFiles(tf)

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
	extractFiles(tf)

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
	extractFiles(tf)

	data, err := os.ReadFile(filepath.Join(sanitizePath(tf.Info.Name), fileB))
	c.NoError(err)
	c.Equal(sampleContent, string(data))
	data, err = os.ReadFile(fileB)
	c.NoError(err)
	c.Equal(existing, string(data), "the file already in the current directory must not be overwritten")
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
