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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/zeebo/bencode"
)

const (
	// notifyWait is how long a test will wait for a notification to be acted on.
	notifyWait = 5 * time.Second

	extractAction = "extract"
	removeAction  = "remove"

	lengthKey = "length"
	fileB     = "b.txt"
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

	// Hold the stop back until the completion has been acted on, so that the two can't be selected between
	go func() {
		select {
		case <-extracted:
		case <-time.After(notifyWait):
		}
		stopped <- nil
	}()
	m.run()
	c.Equal([]string{extractAction, removeAction}, actions)
}

// newTorrentFile builds a torrent whose storage lives in the current directory and holds the supplied content.
func newTorrentFile(t *testing.T, info map[string]any, content string) *tfs.File {
	t.Helper()
	c := check.New(t)
	data, err := bencode.EncodeBytes(map[string]any{"info": info})
	c.NoError(err)
	f, err := tfs.NewFileFromBytes(data)
	c.NoError(err)
	c.NoError(os.WriteFile(f.StoragePath(), []byte(content), 0o600))
	return f
}

// TestExtractFiles verifies that a torrent's files land on disk with their full virtual paths intact. The file info
// the torrent carries holds only base names, so the paths have to come from walking the tree.
func TestExtractFiles(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())

	tf := newTorrentFile(t, map[string]any{
		"name":         "example",
		"piece length": int64(16),
		"pieces":       make([]byte, 40),
		"files": []any{
			map[string]any{lengthKey: int64(12), "path": []any{"sub", "a.txt"}},
			map[string]any{lengthKey: int64(8), "path": []any{fileB}},
		},
	}, "0123456789abcdefghij")
	extractFiles(tf)

	data, err := os.ReadFile(filepath.Join("example", "sub", "a.txt"))
	c.NoError(err)
	c.Equal("0123456789ab", string(data))
	data, err = os.ReadFile(filepath.Join("example", fileB))
	c.NoError(err)
	c.Equal("cdefghij", string(data))
}

// TestExtractFilesSingle verifies that a single-file torrent lands in the current directory rather than in a
// wrapping directory of its own.
func TestExtractFilesSingle(t *testing.T) {
	c := check.New(t)
	t.Chdir(t.TempDir())

	tf := newTorrentFile(t, map[string]any{
		"name":         "example.bin",
		"piece length": int64(16),
		"pieces":       make([]byte, 40),
		lengthKey:      int64(20),
	}, "0123456789abcdefghij")
	extractFiles(tf)

	data, err := os.ReadFile("example.bin")
	c.NoError(err)
	c.Equal("0123456789abcdefghij", string(data))
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
