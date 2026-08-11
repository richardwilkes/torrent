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
	"sort"
	"strconv"
	"sync"
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

// testDialHost stands in for a host we have an outgoing connection attempt to. It comes from the range reserved for
// documentation, so nothing can be listening on it and no dial to it is ever actually made.
const testDialHost = "203.0.113.2"

// TestPeerManagementStopsWhenStoppedBeforeStarting verifies that a stop that arrives before peer management has
// started is still honored, rather than leaving the peer management goroutine running forever.
func TestPeerManagementStopsWhenStoppedBeforeStarting(t *testing.T) {
	d := newTestDispatcher(t)
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
	d := newTestDispatcher(t)
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
	d := newTestDispatcher(t)
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
	d := newTestDispatcher(t)
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
	d := newTestDispatcher(t)
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
	d := newTestDispatcher(t)
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

// TestPeerOrderingsAreConsistent verifies that each of the orderings used to rank peers is a real ordering: no peer
// may rank ahead of one that ranks ahead of it, and a peer that ranks ahead of a second must rank ahead of everything
// that second one ranks ahead of. An ordering that chains its keys such that each of them can only answer in one
// direction has neither property, and leaves the sorted result arbitrary whenever the peers have mixed states.
func TestPeerOrderingsAreConsistent(t *testing.T) {
	now := time.Now()
	older := &peer{created: now.Add(-time.Hour)}
	newer := &peer{created: now}
	for _, one := range []struct {
		less func(a, b *peerData) bool
		name string
	}{
		{name: "rotation", less: func(a, b *peerData) bool { return a.worseForRotation(b, now) }},
		{name: "unchoking", less: func(a, b *peerData) bool { return a.betterForUnchoking(b, now) }},
		{name: "dropping", less: func(a, b *peerData) bool { return a.worseForDropping(b) }},
	} {
		t.Run(one.name, func(t *testing.T) {
			for _, a := range testPeerRankings(now, older, newer) {
				for _, b := range testPeerRankings(now, older, newer) {
					if one.less(a, b) && one.less(b, a) {
						t.Fatalf("%v and %v each rank ahead of the other", a.state, b.state)
					}
				}
			}
			// The transitivity checks are quadratic in the number of peers they're given, so they use a single
			// creation time, leaving the last key of each ordering to act as the tie breaker it is
			all := testPeerRankings(now, newer)
			for _, a := range all {
				for _, b := range all {
					switch {
					case one.less(a, b):
						for _, third := range all {
							if one.less(b, third) && !one.less(a, third) {
								t.Fatalf("%v ranks ahead of %v, which ranks ahead of %v, but not ahead of %v",
									a.state, b.state, third.state, third.state)
							}
						}
					case !one.less(b, a): // Neither ranks ahead of the other, so anything tied with one is tied with both
						for _, third := range all {
							if one.less(b, third) || one.less(third, b) {
								continue
							}
							if one.less(a, third) || one.less(third, a) {
								t.Fatalf("%v and %v are tied, as are %v and %v, but %v and %v are not",
									a.state, b.state, b.state, third.state, a.state, third.state)
							}
						}
					}
				}
			}
		})
	}
}

// testPeerRankings returns peer data covering every combination of the states the peer orderings are keyed on, for
// each of the given peers.
func testPeerRankings(now time.Time, peers ...*peer) []*peerData {
	const combinations = 32
	result := make([]*peerData, 0, combinations*4*len(peers))
	for i := range combinations {
		for _, bytesRead := range []int64{0, 1} {
			for _, bytesWritten := range []int64{0, 1} {
				for _, one := range peers {
					state := peerState{
						amInterested:    i&1 != 0,
						downloading:     i&2 != 0,
						peerChoking:     i&4 != 0,
						peerInterested:  i&8 != 0,
						bytesRead:       bytesRead,
						bytesWritten:    bytesWritten,
						downloadStarted: now,
					}
					if i&16 != 0 {
						state.downloadStarted = now.Add(-2 * maxWaitForChunkDownload)
					}
					result = append(result, &peerData{peer: one, state: state})
				}
			}
		}
	}
	return result
}

// TestUnchokeRankingOrder verifies the priority the unchoke ranking gives to each of the things it looks at, since
// only the peers that come out on top of it are left unchoked.
func TestUnchokeRankingOrder(t *testing.T) {
	c := check.New(t)
	now := time.Now()
	one := &peer{created: now}
	best := peerState{amInterested: true, downloading: true, peerInterested: true, bytesRead: 2, downloadStarted: now}
	// Each of these makes a peer worse than the one before it by exactly one key, in the order the ranking considers
	// them
	worsening := []func(state *peerState){
		func(state *peerState) { state.bytesRead = 1 },
		func(state *peerState) { state.downloadStarted = now.Add(-2 * maxWaitForChunkDownload) },
		func(state *peerState) { state.peerInterested = false },
		func(state *peerState) { state.peerChoking = true },
		func(state *peerState) { state.downloading = false },
		func(state *peerState) { state.amInterested = false },
	}
	expected := make([]*peerData, 0, len(worsening)+1)
	expected = append(expected, &peerData{peer: one, state: best})
	for _, worsen := range worsening {
		state := expected[len(expected)-1].state
		worsen(&state)
		expected = append(expected, &peerData{peer: one, state: state})
	}

	// Sorting any arrangement of them must restore the expected order
	actual := make([]*peerData, len(expected))
	for i := range expected {
		actual[i] = expected[len(expected)-1-i]
	}
	sort.Slice(actual, func(i, j int) bool { return actual[i].betterForUnchoking(actual[j], now) })
	for i := range expected {
		c.Equal(expected[i].state, actual[i].state, "peer %d", i)
	}
}

// TestLeastUsefulPeerIsRotatedOut verifies that the peer given up to make room for an alternate is one that isn't
// doing anything for us, rather than one that is in the middle of downloading a piece.
func TestLeastUsefulPeerIsRotatedOut(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// We already have as many peers as we want, so one has to be given up before an alternate can be added
	client.peersWanted = 2
	downloadingConn, downloading := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(downloadingConn)
	idleConn, idle := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(idleConn)
	downloading.lock.Lock()
	downloading.peerChoking = false
	downloading.lock.Unlock()
	downloading.queuePieceDownload(0)
	c.True(downloading.updateInterest().downloading)
	c.False(idle.updateInterest().amInterested)

	client.adjustPeers()
	checkConnOpen(t, downloadingConn, true)
	checkConnOpen(t, idleConn, false)
}

// TestBusyPeersAreNotRotatedOut verifies that no peer is given up when every one of them is actively downloading a
// piece for us. Fewer peers than concurrent downloads leaves that state with spare download slots, so the rotation is
// reached with nothing worth rotating out: closing one there discards its partial piece and leaves its host free to be
// dialed again on the next pass, since a connection we closed ourselves isn't blocked, repeating the whole cycle on
// every adjustment.
func TestBusyPeersAreNotRotatedOut(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Every peer slot is taken, so an alternate could only be added by giving one of the peers up, and there are still
	// download slots to spare
	client.peersWanted = 2
	c.True(client.peersWanted < client.concurrentDownloads)
	conns := make([]net.Conn, 0, client.peersWanted)
	for i := range cap(conns) {
		conn, p := newTestPeer(t, client)
		conns = append(conns, conn)
		p.lock.Lock()
		p.peerChoking = false
		p.lock.Unlock()
		p.queuePieceDownload(i)
		c.True(p.updateInterest().downloading)
	}
	defer func() {
		for _, conn := range conns {
			xio.CloseIgnoringErrors(conn)
		}
	}()

	client.adjustPeers()
	for _, conn := range conns {
		checkConnOpen(t, conn, true)
	}
}

// TestPeerRankingDoesNotRacePeerCounters verifies that the peer rankings take the byte counts from the state snapshot
// made under each peer's lock, rather than reading them from the peer while its own goroutines are updating them. The
// race is only detected when the tests are run with -race.
func TestPeerRankingDoesNotRacePeerCounters(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Fewer peers are wanted than we have, so both the ranking that finds a peer to make room with and the one that
	// finds a peer to drop have something to sort
	client.peersWanted = 1
	peers := make([]*peer, 0, 3)
	conns := make([]net.Conn, 0, 3)
	for range cap(peers) {
		conn, p := newTestPeer(t, client)
		peers = append(peers, p)
		conns = append(conns, conn)
	}
	defer func() {
		for _, conn := range conns {
			xio.CloseIgnoringErrors(conn)
		}
	}()

	// Update the counters the way each peer's own read and write goroutines do
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				p.lock.Lock()
				p.bytesRead++
				p.bytesWritten++
				p.lock.Unlock()
			}
		})
	}
	for range 10 {
		client.adjustPeers()
		c.True(client.dropPeerIfPossible())
	}
	close(stop)
	wg.Wait()
}

// TestTestDispatchersDoNotProbeForTheExternalIP verifies that the dispatcher these tests are built with reports the
// fixed address it was given rather than looking one up. The lookup issues HTTP requests to outside sites, which a
// unit test has no business doing: it makes the test depend on those sites being reachable and leaves the probe
// running past the end of the test where the network is restricted.
func TestTestDispatchersDoNotProbeForTheExternalIP(t *testing.T) {
	c := check.New(t)
	client := newTestClient(newTestDispatcher(t))
	ip := client.ExternalIP()
	c.NotNil(ip, "no address was reported, so the lookup was not replaced with the fixed one")
	c.Equal(testExternalIP, ip.String(), "the address reported was looked up rather than being the fixed one")
}

// TestHostsInUse verifies that the hosts we're already connected to are collected, so that they aren't connected to a
// second time, and that a dial still on its way to becoming one of them counts just the same. A dial plus the
// handshake exchange that follows it can take longer than the interval between peer adjustments, so a dial that isn't
// accounted for is made again by the next adjustment.
func TestHostsInUse(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn1, p1 := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn1)
	conn2, p2 := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn2)
	registered := []*peerData{{peer: p1}, {peer: p2}}

	existing := client.hostsInUse(registered)
	c.Equal(1, len(existing))
	c.True(existing["127.0.0.1"])

	// A dial that hasn't produced a peer yet is still a host we're using
	c.True(client.startDial(testDialHost))
	c.True(client.hostsInUse(registered)[testDialHost])
	c.False(client.startDial(testDialHost), "a second dial to a host already being dialed must not start")

	// Once the attempt is over, the host is available again
	client.finishDial(testDialHost)
	c.False(client.hostsInUse(registered)[testDialHost])
	c.True(client.startDial(testDialHost))
}

// TestDialInFlightIsNotDialedAgain verifies that peer management leaves a host alone while an earlier connection
// attempt to it is still under way, and dials it once that attempt has finished.
func TestDialInFlightIsNotDialedAgain(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	host, port, accepted := listenForTestDials(t)
	client.tracker.lock.Lock()
	client.tracker.peerAddresses = map[string]int{host: port}
	client.tracker.lock.Unlock()

	// An attempt from an earlier adjustment is still in flight, so this adjustment must not make another
	c.True(client.startDial(host))
	client.adjustPeers()
	select {
	case conn := <-accepted:
		xio.CloseIgnoringErrors(conn)
		t.Fatal("the host was dialed while a connection attempt to it was already in flight")
	case <-time.After(100 * time.Millisecond):
	}

	// With that attempt over, the host is dialed
	client.finishDial(host)
	client.adjustPeers()
	select {
	case conn := <-accepted:
		xio.CloseIgnoringErrors(conn)
	case <-time.After(peerMgmtWait):
		t.Fatal("the host was never dialed")
	}
}

// TestFailedDialIsNotLeftInFlight verifies that a connection attempt that fails releases the host, which would
// otherwise be left looking like it had an attempt in flight forever and never be dialed again.
func TestFailedDialIsNotLeftInFlight(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Nothing is listening on the port, so the dial is refused immediately
	host, port := unusedTestPort(t)
	c.True(client.startDial(host))
	client.connectToPeer(host, port)
	c.False(client.hostsInUse(nil)[host], "a failed connection attempt must not be left in flight")
	c.True(client.startDial(host))
}

// listenForTestDials starts a listener on the loopback interface and returns its host and port, along with the channel
// the connections made to it are delivered on. Closing the listener, which happens when the test ends, delivers a nil
// connection.
func listenForTestDials(t *testing.T) (host string, port int, accepted chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xio.CloseIgnoringErrors(listener) })
	accepted = make(chan net.Conn, 4)
	go func() {
		for {
			one, aerr := listener.Accept()
			if aerr != nil {
				accepted <- nil
				return
			}
			accepted <- one
		}
	}()
	host, port = testAddrHostAndPort(t, listener.Addr())
	return host, port, accepted
}

// unusedTestPort returns a loopback address and a port on it that nothing is listening on, so that a dial to it is
// refused rather than left waiting. The port has to be released before it can be handed back, which leaves a window in
// which another process, or another test binary running at the same time, can bind it; a caller that dialed such a port
// would connect and then sit through a handshake exchange instead of being refused, so the port is checked and a
// different one taken if it turns out to have been claimed.
func unusedTestPort(t *testing.T) (host string, port int) {
	t.Helper()
	for range 10 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host, port = testAddrHostAndPort(t, listener.Addr())
		xio.CloseIgnoringErrors(listener)
		var conn net.Conn
		if conn, err = net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), peerMgmtWait); err != nil {
			return host, port
		}
		xio.CloseIgnoringErrors(conn)
	}
	t.Fatal("unable to find a port that nothing is listening on")
	return "", 0
}

// TestUnusedTestPortIsRefused verifies the premise the failed dial test rests on: that nothing answers on the port it
// is given, so the dial made to it fails rather than reaching a handshake with whatever did answer.
func TestUnusedTestPortIsRefused(t *testing.T) {
	c := check.New(t)
	host, port := unusedTestPort(t)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), peerMgmtWait)
	if err == nil {
		xio.CloseIgnoringErrors(conn)
	}
	c.HasError(err, "something was listening on the port")
}

// testAddrHostAndPort splits an address into the host and port a dial takes.
func testAddrHostAndPort(t *testing.T, addr net.Addr) (host string, port int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	if port, err = strconv.Atoi(portStr); err != nil {
		t.Fatal(err)
	}
	return host, port
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
	d := newTestDispatcher(t)

	client := newTestClient(d)
	c.False(client.InRate.Closed())
	c.False(client.OutRate.Closed())
	client.finish(errStopRequested)
	c.True(client.InRate.Closed())
	c.True(client.OutRate.Closed())

	// A client that never starts because one of its options failed must not leave them behind either
	var partial *Client
	_, err := NewClient(d, newTestTorrentFile(),
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
	d := newTestDispatcher(t)
	client := newTestClient(d)
	notifier := make(chan *Client, 2)
	client.stoppedNotifier = notifier

	// Bounded, so that a delivery that regresses fails the test rather than hanging the suite
	client.notifyStopped(peerMgmtWait)
	select {
	case one := <-notifier:
		c.Equal(client, one)
	case <-time.After(peerMgmtWait):
		t.Fatal("the stopped notification was not delivered")
	}

	// A later attempt must leave it at the single notification already delivered
	client.notifyStopped(peerMgmtWait)
	select {
	case <-notifier:
		t.Fatal("a second stopped notification was delivered")
	default:
	}
}

// TestStoppedNotificationDoesNotBlockForever verifies that a consumer that has stopped listening doesn't strand the
// goroutine delivering the notification.
func TestStoppedNotificationDoesNotBlockForever(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)
	client.stoppedNotifier = make(chan *Client) // Nothing is listening

	waitFor(t, "notifyStopped", func() { client.notifyStopped(100 * time.Millisecond) })
}

// TestTimedOutStopDoesNotNotify verifies that a Stop that gives up waiting doesn't report that we've stopped. The
// shutdown is still under way at that point: peers may still be writing to the storage file and the stopped announce
// may not have been made, so a consumer acting on the notification could take the file away from underneath us. The
// notification the client makes once it has actually finished must also still be delivered.
func TestTimedOutStopDoesNotNotify(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	notifier := make(chan *Client, 2) // Buffered, as the API expects, so a notification would always be accepted
	client.stoppedNotifier = notifier

	// Nothing will ever close the stopped channel, so this Stop can only end by timing out
	waitFor(t, "Stop", func() { client.Stop(100 * time.Millisecond) })
	select {
	case <-notifier:
		t.Fatal("a stopped notification was delivered while the shutdown was still in progress")
	default:
	}

	// The notification made once the client has finished is still delivered
	client.notifyStopped(peerMgmtWait)
	select {
	case one := <-notifier:
		c.Equal(client, one)
	case <-time.After(peerMgmtWait):
		t.Fatal("the stopped notification was never delivered")
	}
}

// TestDownloadCompleteNotification verifies that exactly one download complete notification is delivered.
func TestDownloadCompleteNotification(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	notifier := make(chan *Client, 2)
	client.downloadCompleteNotifier = notifier

	// Bounded, so that a delivery that regresses fails the test rather than hanging the suite
	client.tracker.setState(Seeding)
	select {
	case one := <-notifier:
		c.Equal(client, one)
	case <-time.After(peerMgmtWait):
		t.Fatal("the download complete notification was not delivered")
	}

	// Leaving the seeding state and returning to it must not produce a second notification
	client.tracker.setState(Downloading)
	client.tracker.setState(Seeding)
	select {
	case <-notifier:
		t.Fatal("a second download complete notification was delivered")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDownloadCompleteNotificationDoesNotBlockForever verifies that a consumer that isn't listening can't strand the
// goroutine reporting the completed download. That goroutine is either the client's run goroutine, which would never
// finish starting, or a peer's read goroutine, which would leave finish() waiting on it forever, so the client could
// never stop.
func TestDownloadCompleteNotificationDoesNotBlockForever(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)
	notifier := make(chan *Client) // Unbuffered, with nothing listening
	client.downloadCompleteNotifier = notifier

	waitFor(t, "setState", func() { client.tracker.setState(Seeding) })

	// Accept the notification the fallback goroutine is holding onto, which would otherwise leave it parked for the
	// full downloadCompleteNotifyTimeout after the test has finished
	select {
	case <-notifier:
	case <-time.After(peerMgmtWait):
		t.Fatal("the download complete notification was never offered")
	}
}

// TestDownloadCompleteNotificationReachesALateConsumer verifies that the notification isn't thrown away just because
// the consumer wasn't waiting at the moment the download completed.
func TestDownloadCompleteNotificationReachesALateConsumer(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	notifier := make(chan *Client) // Unbuffered, with nothing listening yet
	client.downloadCompleteNotifier = notifier

	waitFor(t, "setState", func() { client.tracker.setState(Seeding) })
	select {
	case one := <-notifier:
		c.Equal(client, one)
	case <-time.After(peerMgmtWait):
		t.Fatal("the download complete notification was lost")
	}
}

// TestStorageIsNotClearedOutFromUnderPeers verifies that a peer still serving piece requests as the client finishes
// doesn't race with the storage file being closed, and stops serving once it is gone rather than dereferencing a nil
// file. The race is only detected when the tests are run with -race.
func TestStorageIsNotClearedOutFromUnderPeers(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
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
		// Bounded by a deadline as well as by the file going away, so that a regression which leaves the file in
		// place fails the test rather than feeding requests forever and hanging the suite
		deadline := time.Now().Add(peerMgmtWait)
		for client.storageFile() != nil && time.Now().Before(deadline) {
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
	d := newTestDispatcher(t)
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
	d := newTestDispatcher(t)
	client := newTestClient(d)
	// An ID of our own, since the remote presents one of all zeros: leaving ours as the zero value it is created with
	// makes the connection look like one to ourselves, which is refused before the stopping check is ever reached
	for i := range client.id {
		client.id[i] = urlQuerySafeBytes[i%len(urlQuerySafeBytes)]
	}
	client.closeAllPeers()

	conn, remote := newTestConnPair(t)
	defer xio.CloseIgnoringErrors(conn)
	defer xio.CloseIgnoringErrors(remote)
	var peerID dispatcher.PeerID
	_, err := conn.Write(peerID[:])
	c.NoError(err)

	var extensions dispatcher.ProtocolExtensions
	waitFor(t, "HandleConnection", func() {
		client.HandleConnection(remote, client.logger, extensions, client.torrentFile.InfoHash, false)
	})
	c.Equal(0, len(client.currentPeers()))
	c.False(d.GateKeeper().IsAddressBlocked(remote.RemoteAddr()),
		"the connection was refused by a check other than the one for the client stopping")
}

// TestConnectionToOurselvesIsRefused verifies that a remote presenting our own peer ID is severed rather than being
// taken on as a peer. The tracker can hand back our own address, which parsePeers can only filter out when the
// external IP lookup succeeded, so a client that doesn't check the ID connects to itself and occupies two of its
// limited peer slots with the result.
func TestConnectionToOurselvesIsRefused(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	for i := range client.id {
		client.id[i] = urlQuerySafeBytes[i%len(urlQuerySafeBytes)]
	}
	var extensions dispatcher.ProtocolExtensions

	conn, remote := newTestConnPair(t)
	_, err := conn.Write(client.id[:])
	c.NoError(err)
	waitFor(t, "HandleConnection", func() {
		client.HandleConnection(remote, client.logger, extensions, client.torrentFile.InfoHash, false)
	})
	c.Equal(0, len(client.currentPeers()))
	c.True(d.GateKeeper().IsAddressBlocked(remote.RemoteAddr()),
		"the address must be blocked so that peer management doesn't simply dial it again")

	// A remote presenting an ID of its own is still taken on, so the check can't be refusing everything
	otherConn, otherRemote := newTestConnPair(t)
	var peerID dispatcher.PeerID
	_, err = otherConn.Write(peerID[:])
	c.NoError(err)
	go client.HandleConnection(otherRemote, client.logger, extensions, client.torrentFile.InfoHash, false)
	deadline := time.Now().Add(peerMgmtWait)
	for len(client.currentPeers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a peer with an ID of its own was never taken on")
		}
		time.Sleep(time.Millisecond)
	}
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
			d := newTestDispatcher(t)
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
