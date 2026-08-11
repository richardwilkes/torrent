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
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
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
	fileB     = "b.txt"

	// torrentName is the name carried by the torrents the extraction tests build, and therefore the directory a
	// multi-file one is expected to land in.
	torrentName = "example"
	// sampleContent is the storage content of the torrents the extraction tests build. Its length has to agree with
	// the piece length and piece count newTorrentFile supplies.
	sampleContent = "0123456789abcdefghij"
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
	}{
		{
			name:            "the stopped notification races the completion notification",
			state:           torrent.Done,
			completePending: true,
			want:            []string{extractAction, removeAction},
		},
		{
			name:  "the completion notification hasn't been sent yet",
			state: torrent.Done,
			want:  []string{extractAction, removeAction},
		},
		{
			name:             "the completion notification already extracted the files",
			state:            torrent.Done,
			alreadyExtracted: true,
			want:             []string{removeAction},
		},
		{
			name:           "stopped before the download finished",
			state:          torrent.Done,
			remainingBytes: 1024,
			want:           nil,
		},
		{
			name:  "stopped with an error",
			state: torrent.Errored,
			want:  nil,
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
			m.run()
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
	m.run()
	c.True(<-extractedOnCompletion, "the completion notification must extract the files by itself")
	c.Equal([]string{extractAction, removeAction}, actions)
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
		"files": []any{
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
		"files": []any{map[string]any{lengthKey: int64(20), pathKey: []any{fileB}}},
	})
	extractFiles(tf)

	data, err := os.ReadFile(filepath.Join(torrentName, fileB))
	c.NoError(err)
	c.Equal(sampleContent, string(data))
	_, err = os.Stat(fileB)
	c.HasError(err, "the file must not be extracted into the current directory")
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
		{in: ".", want: nil},
	} {
		c.Equal(filepath.Join(one.want...), sanitizePath(one.in), one.in)
	}
}
