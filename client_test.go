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
	"errors"
	"net"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xio"
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

// TestDownloadStalled verifies which download states are considered to be stalled. A peer that has just been asked
// for a piece has yet to send us anything for it, so it must not be treated as if it stopped sending long ago.
func TestDownloadStalled(t *testing.T) {
	c := check.New(t)
	now := time.Now()
	for _, one := range []struct {
		name    string
		state   peerState
		stalled bool
	}{
		{
			name:    "download just started, nothing received yet",
			state:   peerState{downloading: true, downloadStarted: now},
			stalled: false,
		},
		{
			name:    "download started long ago, nothing received",
			state:   peerState{downloading: true, downloadStarted: now.Add(-2 * maxWaitForChunkDownload)},
			stalled: true,
		},
		{
			name: "chunk received recently",
			state: peerState{
				downloading:     true,
				downloadStarted: now.Add(-2 * maxWaitForChunkDownload),
				lastReceived:    now.Add(-time.Second),
			},
			stalled: false,
		},
		{
			name: "no chunk received within the allowed time",
			state: peerState{
				downloading:     true,
				downloadStarted: now.Add(-4 * maxWaitForChunkDownload),
				lastReceived:    now.Add(-2 * maxWaitForChunkDownload),
			},
			stalled: true,
		},
		{
			name: "new download started after a long idle period",
			state: peerState{
				downloading:     true,
				downloadStarted: now,
				lastReceived:    now.Add(-time.Hour),
			},
			stalled: false,
		},
	} {
		c.Equal(one.stalled, one.state.downloadStalled(now), one.name)
	}
}

// TestPeerWithFreshDownloadIsNotBanned verifies that a peer that has just been asked for a piece and hasn't had time
// to deliver its first chunk isn't banned by the next peer adjustment.
func TestPeerWithFreshDownloadIsNotBanned(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The peer has unchoked us and we've asked it for a piece, but it hasn't sent anything yet
	p.lock.Lock()
	p.peerChoking = false
	p.lock.Unlock()
	p.queuePieceDownload(0)
	c.True(p.updateInterest().downloading)

	client.adjustPeers()
	c.False(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()))
	checkConnOpen(t, conn, true)
}

// TestChokingPeerIsNotBanned verifies that a peer choking us mid-download, which is normal protocol behavior, isn't
// banned for it.
func TestChokingPeerIsNotBanned(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// We've been downloading from the peer for a while, but it has now choked us
	p.queuePieceDownload(0)
	p.lock.Lock()
	p.peerChoking = true
	p.downloadStarted = time.Now().Add(-4 * maxWaitForChunkDownload)
	p.lastReceived = time.Now().Add(-2 * maxWaitForChunkDownload)
	p.lock.Unlock()

	client.adjustPeers()
	c.False(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()))
	checkConnOpen(t, conn, true)
}

// TestStalledPeerIsBanned verifies that a peer that is free to send us chunks but doesn't is still banned.
func TestStalledPeerIsBanned(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	p.queuePieceDownload(0)
	p.lock.Lock()
	p.peerChoking = false
	p.downloadStarted = time.Now().Add(-4 * maxWaitForChunkDownload)
	p.lastReceived = time.Now().Add(-2 * maxWaitForChunkDownload)
	p.lock.Unlock()

	client.adjustPeers()
	c.True(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()))
	checkConnOpen(t, conn, false)
}

// TestConnectedHosts verifies that the hosts we're already connected to are collected, so that they aren't connected
// to a second time.
func TestConnectedHosts(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn1, p1 := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn1)
	conn2, p2 := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn2)

	existing := connectedHosts([]*peerData{{peer: p1}, {peer: p2}})
	c.Equal(1, len(existing))
	c.True(existing["127.0.0.1"])
}

// checkConnOpen fails the test if the state of the connection doesn't match what is expected. A connection that has
// been closed by the other side reports EOF, while one that is still open times out with nothing to read.
func checkConnOpen(t *testing.T, conn net.Conn, expected bool) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	open := errors.As(err, &netErr) && netErr.Timeout()
	if open != expected {
		if expected {
			t.Fatalf("connection was closed: %v", err)
		}
		t.Fatal("connection was not closed")
	}
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
