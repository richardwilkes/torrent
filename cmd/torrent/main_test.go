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
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent"
)

const (
	// notifyWait is how long a test will wait for a notification to be acted on.
	notifyWait = 5 * time.Second

	extractAction = "extract"
	removeAction  = "remove"
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
