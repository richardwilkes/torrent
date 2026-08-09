// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package torrent

import (
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent/dispatcher"
)

// peerMgmtWait is how long a test will wait for peer management to react. Anything approaching this means peer
// management is no longer listening for the stop request.
const peerMgmtWait = 5 * time.Second

// TestPeerManagementStopsWhenStoppedBeforeStarting verifies that a stop that arrives before peer management has
// started is still honored, rather than leaving the peer management goroutine running forever.
func TestPeerManagementStopsWhenStoppedBeforeStarting(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)

	// Stop before peer management has been started, which must not block, since there is nothing to wait for
	waitFor(t, "closeAllPeers", func() { client.closeAllPeers() })

	// Startup continues on and starts peer management, which must immediately stop again
	waitFor(t, "startPeerManagement", client.startPeerManagement)
	select {
	case <-peerManagementDone(client):
	case <-time.After(peerMgmtWait):
		t.Fatal("peer management goroutine leaked after the client was stopped")
	}
}

// TestPeerManagementStopsWhenRunning verifies the normal stop path.
func TestPeerManagementStopsWhenRunning(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)

	waitFor(t, "startPeerManagement", client.startPeerManagement)
	done := peerManagementDone(client)
	select {
	case <-done:
		t.Fatal("peer management stopped before it was asked to")
	default:
	}

	waitFor(t, "closeAllPeers", func() { client.closeAllPeers() })
	select {
	case <-done:
	case <-time.After(peerMgmtWait):
		t.Fatal("peer management goroutine leaked after the client was stopped")
	}

	// Stopping a second time must neither panic nor block
	waitFor(t, "second closeAllPeers", func() { client.closeAllPeers() })
}

// TestConnectionsRefusedWhileStopping verifies that the stop state incoming connections check remains in sync with the
// peer management stop.
func TestConnectionsRefusedWhileStopping(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)

	client.peerMgmtLock.Lock()
	stopping := client.peerMgmtStopping
	client.peerMgmtLock.Unlock()
	c.False(stopping)

	client.closeAllPeers()

	client.peerMgmtLock.Lock()
	stopping = client.peerMgmtStopping
	client.peerMgmtLock.Unlock()
	c.True(stopping)
}

// peerManagementDone returns the channel that will be closed when peer management has stopped.
func peerManagementDone(c *Client) chan struct{} {
	c.peerMgmtLock.Lock()
	defer c.peerMgmtLock.Unlock()
	return c.peerMgmtDone
}

// waitFor calls f and fails the test if it doesn't return promptly.
func waitFor(t *testing.T, name string, f func()) {
	t.Helper()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f()
	}()
	select {
	case <-finished:
	case <-time.After(peerMgmtWait):
		t.Fatal(name + " did not return")
	}
}
