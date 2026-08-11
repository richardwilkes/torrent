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
	"math"
	"net"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
)

// rateSettleTime is how long a test will wait on a rate-limited transfer to prove it isn't going to be refused. It
// must be longer than the one second period the limiters use, so that a request that had to queue is reconsidered.
const rateSettleTime = 2500 * time.Millisecond

// TestRateCapsTooSmallForAPieceMessageAreRejected verifies that a cap which would refuse every piece message, and
// therefore tear down the connection to each peer as soon as one arrives, isn't accepted in the first place.
func TestRateCapsTooSmallForAPieceMessageAreRejected(t *testing.T) {
	d := newTestDispatcher(t)
	for _, one := range []struct {
		option func(int) func(*Client) error
		name   string
	}{
		{name: "DownloadCap", option: DownloadCap},
		{name: "UploadCap", option: UploadCap},
	} {
		t.Run(one.name, func(t *testing.T) {
			tc := check.New(t)
			client := newTestClient(d)
			defer client.closeRateLimiters()
			for _, bytesPerSecond := range []int{-1, 0, 1, chunkSize, dispatcher.MinimumRateCap - 1} {
				tc.HasError(one.option(bytesPerSecond)(client), "%d bytes per second must not be accepted",
					bytesPerSecond)
			}
			tc.NoError(one.option(dispatcher.MinimumRateCap)(client))
			tc.NoError(one.option(dispatcher.MinimumRateCap + 1)(client))
		})
	}
}

// TestConcurrentDownloadsLimitsThePeersWeDownloadFrom verifies that the option is the limit on simultaneous downloads
// it says it is. Left to gate nothing but whether more peers were sought, every peer that unchoked us went on to claim
// a piece of its own, so a client configured for one download at a time still pulled pieces from every peer it had.
func TestConcurrentDownloadsLimitsThePeersWeDownloadFrom(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	c.HasError(ConcurrentDownloads(0)(client), "fewer than one download at a time must not be accepted")
	c.NoError(ConcurrentDownloads(2)(client))

	// Three peers, each holding the whole torrent and each free to be asked for a piece
	peers := make([]*peer, 0, 3)
	conns := make([]net.Conn, 0, cap(peers))
	defer func() {
		for _, conn := range conns {
			xio.CloseIgnoringErrors(conn)
		}
	}()
	for range cap(peers) {
		conn, p := newTestPeer(t, client)
		conns = append(conns, conn)
		p.lock.Lock()
		p.peerChoking = false
		p.has = everyTestPiece()
		p.lock.Unlock()
		peers = append(peers, p)
	}
	for _, p := range peers {
		p.startDownloadIfNeeded()
	}
	c.Equal(2, testDownloadingPeerCount(peers), "no more peers than ConcurrentDownloads allows may be downloading")

	// A peer that is already downloading isn't stopped from carrying on with the next piece when it finishes one
	downloading := peers[0]
	if !testPeerIsDownloading(downloading) {
		downloading = peers[2]
	}
	index := testClaimedPiece(downloading)
	downloading.lock.Lock()
	delete(downloading.pieces, index)
	downloading.lock.Unlock()
	client.tracker.markBlockValid(index)
	downloading.startDownloadIfNeeded()
	c.True(testPeerIsDownloading(downloading), "a peer that finished a piece must be able to start the next one")
	c.Equal(2, testDownloadingPeerCount(peers))

	// And the slot a peer gives up goes to one of the peers that was waiting for it
	index = testClaimedPiece(downloading)
	downloading.lock.Lock()
	delete(downloading.pieces, index)
	downloading.lock.Unlock()
	client.tracker.clearDownload(index, downloading)
	c.Equal(1, testDownloadingPeerCount(peers))
	for _, p := range peers {
		if p != downloading {
			p.startDownloadIfNeeded()
		}
	}
	c.Equal(2, testDownloadingPeerCount(peers), "the freed slot must be taken up, and only once")
}

// testDownloadingPeerCount returns how many of the peers are holding a piece.
func testDownloadingPeerCount(peers []*peer) int {
	count := 0
	for _, p := range peers {
		if testPeerIsDownloading(p) {
			count++
		}
	}
	return count
}

// testPeerIsDownloading returns whether the peer is holding a piece.
func testPeerIsDownloading(p *peer) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return len(p.pieces) != 0
}

// testClaimedPiece returns the index of the piece the peer is downloading.
func testClaimedPiece(p *peer) int {
	p.lock.RLock()
	defer p.lock.RUnlock()
	for index := range p.pieces {
		return index
	}
	return -1
}

// TestMinimumRateCapPermitsAPieceMessage verifies that the minimum cap is actually large enough for the largest amount
// we ever ask a rate limiter to account for at once, and that anything smaller really is refused by the limiter, which
// is what makes the minimum necessary.
func TestMinimumRateCapPermitsAPieceMessage(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	defer client.closeRateLimiters()

	c.NoError(DownloadCap(dispatcher.MinimumRateCap)(client))
	c.NoError(UploadCap(dispatcher.MinimumRateCap)(client))
	c.NoError(<-client.InRate.Use(dispatcher.MaxPieceMessageLength))
	c.NoError(<-client.OutRate.Use(dispatcher.MaxPieceMessageLength))

	client.OutRate.SetCap(dispatcher.MinimumRateCap - 1)
	c.HasError(<-client.OutRate.Use(dispatcher.MaxPieceMessageLength))
}

// TestMessagesLargerThanTheCapAreChargedInPieces verifies that a message bigger than the rate cap is still accounted
// for rather than refused. Messages aren't all piece-sized — a bit field grows with the number of pieces in the
// torrent — and a limiter refuses any single amount larger than its cap outright, so charging a whole message at once
// would cost us the connection to a peer that did nothing wrong.
func TestMessagesLargerThanTheCapAreChargedInPieces(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	defer client.closeRateLimiters()
	c.NoError(DownloadCap(dispatcher.MinimumRateCap)(client))

	// The largest message any peer may send — the bit field of the largest torrent that can be loaded, with its
	// length prefix — is far larger than the smallest cap that may be set
	const largest = 4 + 1 + tfs.MaxPieceCount/8
	c.True(largest > dispatcher.MinimumRateCap)
	c.HasError(<-client.InRate.Use(largest), "the limiter must refuse the whole message as a single amount")
	c.NoError(useRate(client.InRate, dispatcher.MinimumRateCap+1), "a message just past the cap must still be charged")

	// With room for all of the pieces in one period, the largest message goes through without waiting
	client.InRate.SetCap(math.MaxInt32)
	c.NoError(useRate(client.InRate, largest))

	// A closed limiter still reports itself, so the peer that was being charged for goes away rather than looping
	client.closeRateLimiters()
	c.HasError(useRate(client.InRate, largest))
}
