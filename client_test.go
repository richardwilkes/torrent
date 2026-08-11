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
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
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

// testPeerHost1, testPeerHost2 and testPeerPort stand in for the remote ends of peers that have to look like they come
// from hosts of their own. They come from the range reserved for documentation, and since a peer we already have is
// never dialed, no connection to them is ever attempted.
const (
	testPeerHost1 = "203.0.113.10"
	testPeerHost2 = "203.0.113.11"
	testPeerPort  = 6881
)

// TestPeerManagementStopsWhenStoppedBeforeStarting verifies that a stop that arrives before peer management has
// started is still honored, rather than leaving the peer management goroutine running forever.
func TestPeerManagementStopsWhenStoppedBeforeStarting(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Stop before peer management has been started, which must not block, since there is nothing to wait for
	waitFor(t, "closeAllPeers", func() { client.closeAllPeers(peerMgmtStopWait) })

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

	waitFor(t, "closeAllPeers", func() { client.closeAllPeers(peerMgmtStopWait) })
	select {
	case <-done:
	case <-time.After(peerMgmtWait):
		t.Fatal("peer management goroutine leaked after the client was stopped")
	}

	// Stopping a second time must neither panic nor block
	waitFor(t, "second closeAllPeers", func() { client.closeAllPeers(peerMgmtStopWait) })
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

	client.closeAllPeers(peerMgmtStopWait)

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

	// We already have as many peers as we want, so one has to be given up before the alternate the tracker gave us can
	// be added
	client.peersWanted = 2
	downloadingConn, downloading := newTestPeerFromHost(t, client, testPeerHost1, testPeerPort)
	defer xio.CloseIgnoringErrors(downloadingConn)
	idleConn, idle := newTestPeerFromHost(t, client, testPeerHost2, testPeerPort)
	defer xio.CloseIgnoringErrors(idleConn)
	host, port, accepted := listenForTestDials(t)
	setTrackerPeerAddresses(client, map[string]int{host: port})
	downloading.lock.Lock()
	downloading.peerChoking = false
	downloading.lock.Unlock()
	downloading.queuePieceDownload(0)
	c.True(downloading.updateInterest().downloading)
	c.False(idle.updateInterest().amInterested)

	client.adjustPeers()
	checkConnOpen(t, downloadingConn, true)
	checkConnOpen(t, idleConn, false)
	select {
	case conn := <-accepted:
		xio.CloseIgnoringErrors(conn)
	case <-time.After(peerMgmtWait):
		t.Fatal("the alternate the peer was given up for was never dialed")
	}
}

// TestPeerIsNotRotatedOutWithoutAnAlternate verifies that no peer is given up when there is nothing to put in its
// place. The tracker's list holds nothing we can use in a small or fully connected swarm, and a peer closed for an
// alternate that doesn't exist is simply lost: the host we closed becomes dialable again on the next pass, since a
// connection we closed ourselves isn't blocked, and ranks worst again as the newest connection with the least read
// from it, so the drop repeats on every adjustment.
func TestPeerIsNotRotatedOutWithoutAnAlternate(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Every peer slot is taken and the tracker hasn't given us any peers at all
	client.peersWanted = 2
	conn1, _ := newTestPeerFromHost(t, client, testPeerHost1, testPeerPort)
	defer xio.CloseIgnoringErrors(conn1)
	conn2, _ := newTestPeerFromHost(t, client, testPeerHost2, testPeerPort)
	defer xio.CloseIgnoringErrors(conn2)

	client.adjustPeers()
	checkConnOpen(t, conn1, true)
	checkConnOpen(t, conn2, true)

	// The only peer the tracker knows about is one we're already connected to, which is no alternate either
	setTrackerPeerAddresses(client, map[string]int{testPeerHost1: testPeerPort})
	client.adjustPeers()
	checkConnOpen(t, conn1, true)
	checkConnOpen(t, conn2, true)
}

// TestPeersAreNotRotatedOutWhileSeeding verifies that the hunt for more peers to download from ends with the download
// itself. Nothing can be downloading once the download is complete, so the count of downloading peers is permanently
// 0: a client that keeps looking for downloaders gives up a connected peer on every pass for the whole seeding period,
// closing the very uploads that period exists to serve.
func TestPeersAreNotRotatedOutWhileSeeding(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Every peer slot is taken, the download is complete, and the tracker has an alternate we could dial
	client.peersWanted = 2
	conn1, _ := newTestPeerFromHost(t, client, testPeerHost1, testPeerPort)
	defer xio.CloseIgnoringErrors(conn1)
	conn2, _ := newTestPeerFromHost(t, client, testPeerHost2, testPeerPort)
	defer xio.CloseIgnoringErrors(conn2)
	host, port, accepted := listenForTestDials(t)
	setTrackerPeerAddresses(client, map[string]int{host: port})
	markTestClientSeeding(client)
	c.True(client.tracker.isDownloadComplete())
	c.False(client.tracker.isSeedingComplete())

	client.adjustPeers()
	checkConnOpen(t, conn1, true)
	checkConnOpen(t, conn2, true)
	select {
	case conn := <-accepted:
		xio.CloseIgnoringErrors(conn)
		t.Fatal("a peer was given up to make room for one there is nothing left to download from")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSeedingFillsFreePeerSlots verifies that a client that has finished downloading still connects to the peers the
// tracker gives it while it has room for them, since each peer added while seeding is another one we can upload to.
func TestSeedingFillsFreePeerSlots(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// There is room for another peer beyond the ones we have, and the download is complete
	client.peersWanted = 3
	conn1, _ := newTestPeerFromHost(t, client, testPeerHost1, testPeerPort)
	defer xio.CloseIgnoringErrors(conn1)
	conn2, _ := newTestPeerFromHost(t, client, testPeerHost2, testPeerPort)
	defer xio.CloseIgnoringErrors(conn2)
	host, port, accepted := listenForTestDials(t)
	setTrackerPeerAddresses(client, map[string]int{host: port})
	markTestClientSeeding(client)

	client.adjustPeers()
	select {
	case conn := <-accepted:
		xio.CloseIgnoringErrors(conn)
	case <-time.After(peerMgmtWait):
		t.Fatal("the peer the tracker gave us was never dialed, even though there was room for it")
	}
	checkConnOpen(t, conn1, true)
	checkConnOpen(t, conn2, true)
}

// markTestClientSeeding puts the tracker into the state it holds once the download has completed and the seeding
// period is under way.
func markTestClientSeeding(client *Client) {
	client.tracker.lock.Lock()
	client.tracker.remainingBytes = 0
	client.tracker.seedExpires = time.Now().Add(time.Hour)
	client.tracker.lock.Unlock()
}

// setTrackerPeerAddresses stands in for the peer addresses an announce would have given the tracker.
func setTrackerPeerAddresses(client *Client, addresses map[string]int) {
	client.tracker.lock.Lock()
	client.tracker.peerAddresses = addresses
	client.tracker.lock.Unlock()
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

	// Every peer slot is taken, so the alternate the tracker gave us could only be added by giving one of the peers up,
	// and there are still download slots to spare
	hosts := []string{testPeerHost1, testPeerHost2}
	client.peersWanted = len(hosts)
	c.True(client.peersWanted < client.concurrentDownloads)
	conns := make([]net.Conn, 0, client.peersWanted)
	for i, host := range hosts {
		conn, p := newTestPeerFromHost(t, client, host, testPeerPort)
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
	host, port, accepted := listenForTestDials(t)
	setTrackerPeerAddresses(client, map[string]int{host: port})

	client.adjustPeers()
	for _, conn := range conns {
		checkConnOpen(t, conn, true)
	}
	select {
	case conn := <-accepted:
		xio.CloseIgnoringErrors(conn)
		t.Fatal("a peer delivering a piece was given up for an alternate")
	case <-time.After(100 * time.Millisecond):
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
	setTrackerPeerAddresses(client, map[string]int{host: port})

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

// TestAStorageFailureLeavesTheTorrentErrored verifies that the failure a peer records when it can't store a piece is
// what the torrent then stops for, so that a download which couldn't be written is reported as having failed rather
// than as having finished normally.
func TestAStorageFailureLeavesTheTorrentErrored(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Storage that has been closed because we're already shutting down isn't a failure. Peer goroutines the wait group
	// doesn't track can still be draining work at that point, and the shutdown that closed it is what they're behind.
	client.failWithStorageError(errs.NewWithCause("unable to write piece", errStorageClosed))
	c.NoError(client.fatalError())

	// The first failure is the one kept, since it is the one that explains what went wrong
	first := errors.New("no space left on device")
	client.failWithStorageError(errs.NewWithCause("unable to write piece", first))
	client.failWithStorageError(errs.New("unable to write piece"))
	c.True(errors.Is(client.fatalError(), first), "the failure that stopped the torrent must be the one reported")

	waitFor(t, "finish", func() { client.finish(client.fatalError()) })
	c.Equal(Errored, client.Status().State, "a torrent that stopped for a storage failure must not report success")
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
	// A peer starts out choked, and processPieceRequests discards everything a choked peer asks for before it ever
	// reaches the storage, so without this nothing here would touch the file the test is about
	p.setChoked(false)

	// Drain whatever the peer queues up for writing, so that it never blocks on a full queue, handing each message to
	// the test as it goes so that requests which were actually served can be told apart from ones that were discarded
	drained := make(chan struct{})
	written := make(chan []byte, 1)
	go func() {
		defer close(drained)
		for buffer := range p.writeQueue {
			select {
			case written <- buffer:
			default:
			}
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

	// Serve one request while the storage is still in place, so that the read path is known to be reachable before the
	// file is taken away. Without this, a shutdown that wins the race feeds nothing and the test proves nothing.
	requests <- &pieceRequest{index: 1, length: chunkSize}
	select {
	case buffer := <-written:
		c.Equal(13+chunkSize, len(buffer))
		c.Equal(pieceID, buffer[4])
		c.Equal(uint32(1), binary.BigEndian.Uint32(buffer[5:9]))
		c.Equal(testStorageBytes(1, 0, chunkSize), buffer[13:])
	case <-time.After(peerMgmtWait):
		t.Fatal("the piece request was never served, so the storage was never read")
	}

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

// TestStopHonorsItsTimeoutWhilePeerManagementLags verifies that the timeout Stop is given bounds the whole stop, the
// wait for the peer management goroutine included. A pass through peer management sends piece requests into peers'
// bounded write queues, which drain at rate-limited speed, so it can realistically take far longer to notice the stop
// than the caller allowed for; waiting for it on a schedule of its own breaks the contract Stop documents.
func TestStopHonorsItsTimeoutWhilePeerManagementLags(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// Peer management that never notices the stop: nothing ever closes this, nor the channel Stop watches for the
	// shutdown having finished
	client.peerMgmtLock.Lock()
	client.peerMgmtDone = make(chan struct{})
	client.peerMgmtLock.Unlock()

	const timeout = 100 * time.Millisecond
	started := time.Now()
	stopped := make(chan time.Duration, 1)
	go func() {
		client.Stop(timeout)
		stopped <- time.Since(started)
	}()
	select {
	case elapsed := <-stopped:
		c.True(elapsed < peerMgmtWait, "Stop was given %v but took %v", timeout, elapsed)
	case <-time.After(peerMgmtWait):
		t.Fatal("Stop waited on peer management rather than honoring the timeout it was given")
	}
}

// TestPeerManagementLogsThroughTheClientLogger verifies that the debug output peer management and dialing produce goes
// to the client's logger rather than the process default one. A host application that pointed the dispatcher's logging
// somewhere of its own with LogTo would otherwise never see these lines, and they'd lack the per-torrent context every
// other line the client logs carries.
func TestPeerManagementLogsThroughTheClientLogger(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	sink := &logSink{}
	client.logger = slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defaultSink := captureDefaultLogger(t)

	client.adjustPeers()
	c.Contains(sink.contents(), `msg="managing peers"`)

	// A port nothing is listening on, so the dial fails at once rather than making the test wait out peerDialTimeout
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err)
	addr, ok := listener.Addr().(*net.TCPAddr)
	c.True(ok)
	c.NoError(listener.Close())
	client.connectToPeer(addr.IP.String(), addr.Port)
	c.Contains(sink.contents(), `msg="dialing peer"`)

	c.NotContains(defaultSink.contents(), "managing peers",
		"peer management must log through the client's logger, not the process default one")
	c.NotContains(defaultSink.contents(), "dialing peer",
		"dialing must log through the client's logger, not the process default one")
}

// captureDefaultLogger replaces the process default logger with one that records what it is given for the duration of
// the test, so that a test can tell whether anything reached it, and puts the original back afterwards.
func captureDefaultLogger(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// logSink collects log output. Writes are serialized because a client logs from its own goroutines while the test that
// made it is reading what has been logged so far.
type logSink struct {
	buffer strings.Builder
	lock   sync.Mutex
}

func (s *logSink) Write(p []byte) (int, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.buffer.Write(p)
}

// contents returns everything logged so far.
func (s *logSink) contents() string {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.buffer.String()
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
	client.closeAllPeers(peerMgmtStopWait)

	conn, remote := newTestConnPair(t)
	defer xio.CloseIgnoringErrors(conn)
	defer xio.CloseIgnoringErrors(remote)
	var peerID dispatcher.PeerID
	_, err := conn.Write(peerID[:])
	c.NoError(err)

	var extensions dispatcher.ProtocolExtensions
	waitFor(t, "HandleConnection", func() {
		client.HandleConnection(remote, client.logger, extensions, client.torrentFile.InfoHash, false, nil)
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
		client.HandleConnection(remote, client.logger, extensions, client.torrentFile.InfoHash, false, nil)
	})
	c.Equal(0, len(client.currentPeers()))
	c.True(d.GateKeeper().IsAddressBlocked(remote.RemoteAddr()),
		"the address must be blocked so that peer management doesn't simply dial it again")

	// A remote presenting an ID of its own is still taken on, so the check can't be refusing everything
	otherConn, otherRemote := newTestConnPair(t)
	var peerID dispatcher.PeerID
	_, err = otherConn.Write(peerID[:])
	c.NoError(err)
	go client.HandleConnection(otherRemote, client.logger, extensions, client.torrentFile.InfoHash, false, nil)
	deadline := time.Now().Add(peerMgmtWait)
	for len(client.currentPeers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a peer with an ID of its own was never taken on")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHandshakeCompletionIsReportedAfterThePeerIDIsRead verifies that the dispatcher isn't told the handshake is over
// until the peer ID read that finishes it has actually completed, and that it is told before the peer session begins.
// Reporting any earlier hands back the pending handshake slot while a read with a deadline of its own is still
// outstanding, which is precisely the read a remote that wants to hold a socket and a goroutine without being counted
// against anything simply never satisfies; reporting any later holds the slot for a session the remote's keep-alives
// can sustain for as long as it likes.
func TestHandshakeCompletionIsReportedAfterThePeerIDIsRead(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	// An ID of our own, so that the all zeros ID the remote presents isn't taken for a connection to ourselves
	for i := range client.id {
		client.id[i] = urlQuerySafeBytes[i%len(urlQuerySafeBytes)]
	}
	var extensions dispatcher.ProtocolExtensions

	conn, remote := newTestConnPair(t)
	reported := make(chan struct{}, 1)
	go client.HandleConnection(remote, client.logger, extensions, client.torrentFile.InfoHash, false,
		func() { reported <- struct{}{} })

	// Nothing has been sent, so the handler is parked on the peer ID read with the handshake still unfinished
	select {
	case <-reported:
		t.Fatal("the handshake was reported finished while the peer ID read was still outstanding")
	case <-time.After(100 * time.Millisecond):
	}

	var peerID dispatcher.PeerID
	_, err := conn.Write(peerID[:])
	c.NoError(err)
	select {
	case <-reported:
	case <-time.After(peerMgmtWait):
		t.Fatal("the handshake was never reported finished")
	}

	// The peer session, which is what the slot must not be held across, only starts once the report has been made
	deadline := time.Now().Add(peerMgmtWait)
	for len(client.currentPeers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the peer session never started")
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

			waitFor(t, "closeAllPeers", func() { client.closeAllPeers(peerMgmtStopWait) })
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
