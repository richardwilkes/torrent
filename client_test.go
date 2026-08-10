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
	"github.com/richardwilkes/toolbox/v2/errs"
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

// TestRateLimitersAreClosed verifies that our limiters are released from the dispatcher's limiter tree, which would
// otherwise keep traversing them on every tick for the life of the dispatcher.
func TestRateLimitersAreClosed(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()

	client := newTestClient(d)
	c.False(client.InRate.Closed())
	c.False(client.OutRate.Closed())
	client.finish(errStopRequested)
	c.True(client.InRate.Closed())
	c.True(client.OutRate.Closed())

	// A client that never starts because one of its options failed must not leave them behind either
	var partial *Client
	_, err = NewClient(d, newTestTorrentFile(),
		func(one *Client) error {
			partial = one
			return nil
		},
		func(_ *Client) error {
			return errs.New("option failed")
		})
	c.HasError(err)
	c.NotNil(partial)
	c.True(partial.InRate.Closed())
	c.True(partial.OutRate.Closed())
}

// TestStoppedNotification verifies that exactly one stopped notification is delivered.
func TestStoppedNotification(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	notifier := make(chan *Client, 2)
	client.stoppedNotifier = notifier

	client.notifyStopped(peerMgmtWait)
	c.Equal(client, <-notifier)

	// Both the run and the timed-out Stop paths must leave it at the single notification already delivered
	client.notifyStopped(peerMgmtWait)
	client.notifyStopped(0)
	select {
	case <-notifier:
		t.Fatal("a second stopped notification was delivered")
	default:
	}
}

// TestStoppedNotificationDoesNotBlockForever verifies that a consumer that has stopped listening doesn't strand the
// goroutine delivering the notification.
func TestStoppedNotificationDoesNotBlockForever(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	client.stoppedNotifier = make(chan *Client) // Nothing is listening

	waitFor(t, "notifyStopped", func() { client.notifyStopped(100 * time.Millisecond) })
}

// TestStoppedNotificationIsNotLost verifies that the non-blocking attempt made when Stop times out doesn't consume the
// notification when there is nothing there to accept it.
func TestStoppedNotificationIsNotLost(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	notifier := make(chan *Client) // Unbuffered, with nothing listening yet
	client.stoppedNotifier = notifier

	waitFor(t, "notifyStopped", func() { client.notifyStopped(0) })
	go client.notifyStopped(peerMgmtWait)
	select {
	case one := <-notifier:
		c.Equal(client, one)
	case <-time.After(peerMgmtWait):
		t.Fatal("the stopped notification was lost")
	}
}

// TestStorageIsNotClearedOutFromUnderPeers verifies that a peer still serving piece requests as the client finishes
// doesn't race with the storage file being closed, and stops serving once it is gone rather than dereferencing a nil
// file. The race is only detected when the tests are run with -race.
func TestStorageIsNotClearedOutFromUnderPeers(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	client.file = newTestStorage(t, client)
	markTestPiecesAvailable(client, 1)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// Drain whatever the peer queues up for writing, so that it never blocks on a full queue
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range p.writeQueue { //nolint:revive // Draining the queue is all that is wanted
		}
	}()

	// processPieceRequests isn't tracked by the client's peer wait group, so it is still running when the client
	// closes the storage file
	requests := make(chan *pieceRequest)
	served := make(chan struct{})
	go func() {
		defer close(served)
		p.processPieceRequests(requests)
	}()
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		defer close(requests)
		for client.storageFile() != nil {
			requests <- &pieceRequest{index: 1, length: chunkSize}
		}
	}()

	client.finish(errStopRequested)
	c.Nil(client.storageFile())
	<-fed
	<-served
	close(p.writeQueue)
	<-drained
}

// TestStopIsIdempotentOnceStopped verifies that the stopped state is actually recorded, so that a Stop arriving after
// the client has already stopped returns immediately rather than running the shutdown path a second time.
func TestStopIsIdempotentOnceStopped(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	c.False(clientStopped(client))

	client.finish(errStopRequested)
	c.True(clientStopped(client))

	// Nothing will ever close the stopped channel now, so a Stop that ignored the stopped state would block for the
	// full timeout
	waitFor(t, "Stop", func() { client.Stop(time.Minute) })
	client.lock.RLock()
	requested := client.stopRequested
	client.lock.RUnlock()
	c.False(requested, "Stop must not re-run the shutdown path once the client has stopped")
}

// TestPeerIsNotLeftBehindWhileStopping verifies that a connection arriving while the client is stopping doesn't leave
// a peer behind in the map, since nothing would ever drain its write queue or close its connection.
func TestPeerIsNotLeftBehindWhileStopping(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	client.closeAllPeers()

	conn, remote := newTestConnPair(t)
	defer xio.CloseIgnoringErrors(conn)
	defer xio.CloseIgnoringErrors(remote)
	var peerID dispatcher.PeerID
	_, err = conn.Write(peerID[:])
	c.NoError(err)

	var extensions dispatcher.ProtocolExtensions
	waitFor(t, "HandleConnection", func() {
		client.HandleConnection(remote, client.logger, extensions, client.torrentFile.InfoHash, false)
	})
	c.Equal(0, len(client.currentPeers()))
}

// TestPeerManagementIntervalsAreIndependent verifies that each of peer management's two periodic tasks is driven by
// its own timer. Restarting both timers on every pass through the loop leaves the longer of the two intervals unable
// to ever elapse, making its task unreachable.
func TestPeerManagementIntervalsAreIndependent(t *testing.T) {
	for _, one := range []struct {
		pick func(clearExpired, adjust chan time.Time) chan time.Time
		name string
	}{
		{
			name: "clear expired downloads",
			pick: func(clearExpired, _ chan time.Time) chan time.Time { return clearExpired },
		},
		{
			name: "adjust peers",
			pick: func(_, adjust chan time.Time) chan time.Time { return adjust },
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			d, err := dispatcher.NewDispatcher()
			c.NoError(err)
			defer d.Stop()
			client := newTestClient(d)
			conn, p := newTestPeer(t, client)
			defer xio.CloseIgnoringErrors(conn)

			ready := make(chan struct{})
			done := make(chan struct{})
			client.peerMgmtLock.Lock()
			client.peerMgmtDone = done
			client.peerMgmtLock.Unlock()
			clearExpired := make(chan time.Time)
			adjust := make(chan time.Time)
			go client.managePeers(ready, done, clearExpired, adjust)
			<-ready

			// Only the tick under test is ever delivered, so it is the only thing that can clear the expired download
			p.lock.Lock()
			p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Minute)}
			p.lock.Unlock()
			select {
			case one.pick(clearExpired, adjust) <- time.Now():
			case <-time.After(peerMgmtWait):
				t.Fatal("the " + one.name + " tick was not accepted")
			}
			waitForDownloadsCleared(t, p)

			waitFor(t, "closeAllPeers", func() { client.closeAllPeers() })
			select {
			case <-done:
			case <-time.After(peerMgmtWait):
				t.Fatal("peer management goroutine leaked after the client was stopped")
			}
		})
	}
}

// clientStopped returns whether the client has recorded that it finished stopping.
func clientStopped(c *Client) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.stopped
}

// waitForDownloadsCleared fails the test if the peer's expired downloads aren't cleared promptly.
func waitForDownloadsCleared(t *testing.T, p *peer) {
	t.Helper()
	deadline := time.Now().Add(peerMgmtWait)
	for {
		p.lock.RLock()
		count := len(p.pieces)
		p.lock.RUnlock()
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the expired download was not cleared")
		}
		time.Sleep(time.Millisecond)
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
