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
	"crypto/sha1" //nolint:gosec // The spec requires sha1
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/container/fixedbits"
	"github.com/richardwilkes/torrent/container/spanlist"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
)

const (
	testPieceCount = 4
	sha1Size       = 20

	// testExternalIP is the external address the dispatchers the tests are built with report. It comes from the range
	// reserved for documentation, so no real lookup could ever return it, which is what lets a test tell the fixed
	// address apart from one that was actually looked up.
	testExternalIP = "203.0.113.1"
)

func TestValidMessageLength(t *testing.T) {
	c := check.New(t)
	for _, one := range []struct {
		name    string
		valid   []uint32
		invalid []uint32
		id      byte
	}{
		{name: "choke", id: chokeID, valid: []uint32{1}, invalid: []uint32{2, 5, 13}},
		{name: "unchoke", id: unchokeID, valid: []uint32{1}, invalid: []uint32{2, 5, 13}},
		{name: "interested", id: interestedID, valid: []uint32{1}, invalid: []uint32{2, 5, 13}},
		{name: "not interested", id: notInterestedID, valid: []uint32{1}, invalid: []uint32{2, 5, 13}},
		{name: "have", id: haveID, valid: []uint32{5}, invalid: []uint32{1, 2, 3, 4, 6, 13}},
		{name: "bit field", id: bitFieldID, valid: []uint32{1, 2, 100}, invalid: nil},
		{name: "request", id: requestID, valid: []uint32{13}, invalid: []uint32{1, 5, 9, 12, 14}},
		{name: "piece", id: pieceID, valid: []uint32{10, 9 + chunkSize}, invalid: []uint32{1, 5, 8, 9}},
		{name: "cancel", id: cancelID, valid: []uint32{13}, invalid: []uint32{1, 5, 9, 12, 14}},
		{name: "port", id: portID, valid: []uint32{3}, invalid: []uint32{1, 2, 4}},
		{name: "unknown", id: 200, valid: []uint32{1, 2, 3, 4, 5}, invalid: nil},
	} {
		for _, length := range one.valid {
			c.True(validMessageLength(one.id, length), "%s with a length of %d should be valid", one.name, length)
		}
		for _, length := range one.invalid {
			c.False(validMessageLength(one.id, length), "%s with a length of %d should be invalid", one.name, length)
		}
	}
}

// TestMalformedMessagesAreRejected verifies that a peer sending a message whose length is too short for its ID is
// disconnected rather than causing a slice bounds panic while the message is decoded.
func TestMalformedMessagesAreRejected(t *testing.T) {
	d := newTestDispatcher(t)
	for _, one := range []struct {
		name    string
		message []byte
	}{
		{name: "have with a truncated index", message: newTestMessage(haveID, 0, 0)},
		{name: "have with no payload", message: newTestMessage(haveID)},
		{name: "request with a truncated payload", message: newTestMessage(requestID, 0, 0, 0, 0)},
		{name: "piece with a truncated header", message: newTestMessage(pieceID, 0, 0, 0, 0)},
		{name: "piece with no payload", message: newTestMessage(pieceID)},
		{name: "cancel with a truncated payload", message: newTestMessage(cancelID, 0, 0, 0, 0)},
		{name: "cancel with no payload", message: newTestMessage(cancelID)},
		{name: "choke with extra data", message: newTestMessage(chokeID, 0, 0, 0, 0)},
		{name: "port with a truncated port", message: newTestMessage(portID, 0)},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			conn, _, done := startTestPeer(t, newTestClient(d))
			defer xio.CloseIgnoringErrors(conn)
			_, werr := conn.Write(one.message)
			c.NoError(werr)
			// The rejection must happen while decoding the message, well before the read deadline for
			// the next message could expire and close the connection for an unrelated reason.
			select {
			case <-done:
			case <-time.After(msgReadDeadline / 2):
				t.Fatal("peer did not reject the malformed message")
			}
		})
	}
}

// TestWellFormedMessagesAreAccepted verifies that the message length validation doesn't reject valid messages.
func TestWellFormedMessagesAreAccepted(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	conn, _, done := startTestPeer(t, newTestClient(d))
	defer xio.CloseIgnoringErrors(conn)

	// Tell the peer we have piece 0, which should result in it telling us it is interested
	_, err := conn.Write(newTestMessage(haveID, 0, 0, 0, 0))
	c.NoError(err)
	c.NoError(conn.SetReadDeadline(time.Now().Add(msgReadDeadline)))
	buffer := make([]byte, 5)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestMessage(interestedID), buffer)

	select {
	case <-done:
		t.Fatal("peer closed the connection for well-formed messages")
	default:
	}
}

// TestTransferTotalsReachTheTracker verifies that the bytes a peer moves are recorded for the tracker, which reports
// them in every announce. Counters that never leave the peer leave every announce claiming nothing was transferred.
func TestTransferTotalsReachTheTracker(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	c.Equal(int64(0), client.tracker.downloadedBytes.Load())
	c.Equal(int64(0), client.tracker.uploadedBytes.Load())

	conn, _, _ := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// Tell the peer we have piece 0, which it answers with an interested message
	message := newTestMessage(haveID, 0, 0, 0, 0)
	_, err := conn.Write(message)
	c.NoError(err)
	c.NoError(conn.SetReadDeadline(time.Now().Add(msgReadDeadline)))
	response := make([]byte, 5)
	_, err = io.ReadFull(conn, response)
	c.NoError(err)
	c.Equal(newTestMessage(interestedID), response)

	// The counters are updated once each transfer completes, so they may not have caught up yet
	deadline := time.Now().Add(peerMgmtWait)
	for {
		downloaded := client.tracker.downloadedBytes.Load()
		uploaded := client.tracker.uploadedBytes.Load()
		if downloaded >= int64(len(message)) && uploaded >= int64(len(response)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tracker was told of %d bytes downloaded and %d uploaded, expected at least %d and %d",
				downloaded, uploaded, len(message), len(response))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestBitFieldLargerThanTheRateCapIsAccepted verifies that a peer isn't dropped over the very first thing it says.
// A bit field grows with the number of pieces in the torrent and outgrows the smallest permitted cap at around
// 131,000 pieces, so charging the whole message to the limiter in one go would disconnect every peer of such a
// torrent as soon as its bit field arrived, leaving it with no one to talk to.
func TestBitFieldLargerThanTheRateCapIsAccepted(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)

	// Enough pieces that the bit field message is larger than the smallest cap that may be set
	const pieceCount = 140000
	client := newTestClientForTorrent(d, newTestTorrentFileWithPieces(pieceCount))
	defer client.closeRateLimiters()
	c.NoError(DownloadCap(dispatcher.MinimumRateCap)(client))

	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	bits := make([]byte, p.has.ByteLength())
	for i := range bits {
		bits[i] = 0xFF
	}
	c.True(len(bits)+5 > dispatcher.MinimumRateCap, "the bit field message must exceed the cap to be a test of it")

	// The peer has everything and we have nothing, so it is answered with an interested message
	_, err := conn.Write(newTestMessage(bitFieldID, bits...))
	c.NoError(err)
	c.NoError(conn.SetReadDeadline(time.Now().Add(msgReadDeadline)))
	response := make([]byte, 5)
	_, err = io.ReadFull(conn, response)
	c.NoError(err)
	c.Equal(newTestMessage(interestedID), response)

	// Being charged for the message takes more than one period at this cap, so the connection has to survive the wait
	select {
	case <-done:
		t.Fatal("the peer was dropped over a bit field larger than the rate cap")
	case <-time.After(rateSettleTime):
	}
}

// TestBitFieldSpareBitsAreIgnored verifies that a peer that sets the spare bits at the end of its bit field, which
// correspond to pieces that don't exist, can't get us to request a piece index beyond the end of the torrent. Doing so
// would panic when the piece it returned was validated against the piece hashes.
func TestBitFieldSpareBitsAreIgnored(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	// We already have every piece in the torrent, so only the spare bits could be seen as something to download
	client.tracker.lock.Lock()
	for i := range testPieceCount {
		client.tracker.have.Set(i)
	}
	client.tracker.lock.Unlock()

	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	expectBitField(t, conn, 0xF0)

	// The peer claims to have everything, including the pieces that don't exist
	c.Equal(1, p.has.ByteLength())
	_, err := conn.Write(newTestMessage(bitFieldID, 0xFF))
	c.NoError(err)
	_, err = conn.Write(newTestMessage(unchokeID))
	c.NoError(err)

	// Nothing should be requested, since there is nothing left to download
	c.NoError(conn.SetReadDeadline(time.Now().Add(time.Second)))
	buffer := make([]byte, 64)
	if n, rerr := conn.Read(buffer); rerr == nil {
		t.Fatalf("the peer had nothing to say, but sent %v", buffer[:n])
	}
	p.lock.RLock()
	requested := slices.Sorted(maps.Keys(p.pieces))
	p.lock.RUnlock()
	c.Equal(0, len(requested), "requested piece indexes: %v", requested)

	select {
	case <-done:
		t.Fatal("peer closed the connection for well-formed messages")
	default:
	}
}

// TestBitFieldIsSentToNewPeers verifies that a peer is told which pieces we already have as soon as it connects. The
// only other advertisement we make is for pieces that finish while the peer is connected, so a peer that connects to a
// client that already has pieces — every peer that connects to one that is seeding — would otherwise see us as having
// nothing and never ask us for anything at all.
func TestBitFieldIsSentToNewPeers(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)
	markTestPiecesAvailable(client, 1, 3)
	conn, _, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The bit field must be the first thing the peer says, and must name exactly the pieces we have
	expectBitField(t, conn, 0x50)

	select {
	case <-done:
		t.Fatal("peer closed the connection")
	default:
	}
}

// TestNoBitFieldIsSentWhenWeHaveNothing verifies that a client with nothing to offer leaves the bit field out, which
// the protocol allows, rather than sending an empty one.
func TestNoBitFieldIsSentWhenWeHaveNothing(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	conn, _, _ := startTestPeer(t, newTestClient(d))
	defer xio.CloseIgnoringErrors(conn)

	// Tell the peer we have a piece, which makes it interested. That message arriving first proves no bit field came
	// ahead of it.
	_, err := conn.Write(newTestMessage(haveID, 0, 0, 0, 0))
	c.NoError(err)
	c.NoError(conn.SetReadDeadline(time.Now().Add(msgReadDeadline)))
	buffer := make([]byte, 5)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestMessage(interestedID), buffer)
}

// expectBitField fails the test unless the next message from the peer is a bit field holding the given bytes.
func expectBitField(t *testing.T, conn net.Conn, bits ...byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(msgReadDeadline)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5+len(bits))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatal(err)
	}
	check.New(t).Equal(newTestMessage(bitFieldID, bits...), buffer, "the peer must announce the pieces we have")
}

// TestDuplicateChunksDoNotRenewTheDownloadDeadline verifies that a chunk carrying nothing we don't already have
// doesn't count as progress. A peer that dribbles out duplicate chunks would otherwise hold onto a piece for as long
// as it liked, at almost no cost to itself, and no other peer could take that piece while it did.
func TestDuplicateChunksDoNotRenewTheDownloadDeadline(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	// Pieces of more than one chunk, since a chunk has to be one of the blocks we asked for and the only such block a
	// single chunk piece has completes the piece
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.queuePieceDownload(0)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	c.NotNil(one)

	// The first chunk of the piece arrives, which is progress
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize)))
	deadline, received := downloadDeadline(p, one)

	// The same chunk arrives again, carrying nothing new
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize)))
	repeated, repeatedReceived := downloadDeadline(p, one)
	c.Equal(deadline, repeated, "a duplicate chunk must not renew the deadline for the piece")
	c.Equal(received, repeatedReceived, "a duplicate chunk must not count as progress for the peer")

	// A chunk holding data we don't have yet does renew both. They are moved to a marker rather than compared against
	// the values the duplicate above left behind, since a platform whose clock has a coarse granularity can read the
	// same instant for two calls made back to back, which would leave a renewal indistinguishable from no renewal at
	// all. The marker sits in the future, so that the piece doesn't look expired while the chunk is being taken in and
	// have us give up on the peer, but nearer than the deadline a renewal sets, which only ever moves forward.
	marker := time.Now().Add(time.Second)
	one.lock.Lock()
	one.timeout = marker
	one.lock.Unlock()
	p.lock.Lock()
	p.lastReceived = marker
	p.lock.Unlock()
	c.NoError(p.receivedChunk(0, chunkSize, testStorageBytes(0, chunkSize, chunkSize)))
	renewed, renewedReceived := downloadDeadline(p, one)
	c.True(renewed.After(marker), "a chunk with new data must renew the deadline for the piece")
	c.True(renewedReceived.Before(marker), "a chunk with new data must count as progress for the peer")

	// With the deadline no longer being renewed, the piece is given up. This peer is choking us, so it was told to
	// discard what we asked it for and having nothing to show for the piece isn't something it can be blamed for: the
	// connection is kept.
	one.lock.Lock()
	one.timeout = time.Now().Add(-time.Second)
	one.lock.Unlock()
	p.clearExpiredDownloads()
	p.lock.RLock()
	remaining := len(p.pieces)
	bailing := p.bail
	p.lock.RUnlock()
	c.Equal(0, remaining, "the piece must be given up once its deadline passes")
	c.False(bailing, "a peer that is choking us must not be dropped for failing to deliver")

	// The same expiry for a peer that was free to send us data all along is what drops it
	p.lock.Lock()
	p.peerChoking = false
	p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Second), requested: true}
	p.lock.Unlock()
	p.clearExpiredDownloads()
	p.lock.RLock()
	bailing = p.bail
	p.lock.RUnlock()
	c.True(bailing, "a peer that was free to deliver and didn't must be dropped")
}

// TestADownloadIsNotHeldAgainstAPeerUntilTheRequestsGoOut verifies that the clocks deciding whether a peer is still
// delivering a piece start when the messages asking for it reach the wire rather than when the piece was claimed. The
// requests wait their turn on the shared write queue, which for a peer we're also uploading to under a cap is backed
// up for tens of seconds: anchored at the claim, the deadline expires while they are still queued, which releases the
// piece and — the peer has no reason to be choking us — gives up on a peer that was never asked for anything, and has
// the next peer adjustment ban its address for the same non-offense.
func TestADownloadIsNotHeldAgainstAPeerUntilTheRequestsGoOut(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The peer isn't choking us and has been asked for a piece, but nothing has gone out yet, which is what not having
	// started the delivery goroutine stands in for. Enough time has passed that both the deadline for the piece and
	// the stall clock would have run out had they started at the claim.
	p.lock.Lock()
	p.peerChoking = false
	p.lock.Unlock()
	p.queuePieceDownload(0)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	c.NotNil(one)
	one.lock.Lock()
	one.timeout = time.Now().Add(-time.Second)
	one.lock.Unlock()
	p.lock.Lock()
	p.downloadStarted = time.Now().Add(-4 * maxWaitForChunkDownload)
	p.lock.Unlock()

	state := p.updateInterest()
	c.False(state.downloadStalled(time.Now()), "a peer that hasn't been asked for anything yet must not count as stalled")
	p.clearExpiredDownloads()
	p.lock.RLock()
	held := len(p.pieces)
	bailing := p.bail
	p.lock.RUnlock()
	c.Equal(1, held, "a piece whose requests are still queued up must not be given up")
	c.False(bailing, "a peer must not be dropped for requests that never left our own write queue")
	client.adjustPeers()
	c.False(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()),
		"a peer must not be banned for requests that never left our own write queue")
	checkConnOpen(t, conn, true)

	// Once the request actually goes out, both clocks start from there and the piece is held to them again
	p.noteRequestSent(newRequestMessage(0, 0, chunkSize))
	one.lock.RLock()
	requested := one.requested
	deadline := one.timeout
	one.lock.RUnlock()
	c.True(requested)
	c.True(deadline.After(time.Now()), "the deadline for the piece must start when the request goes out")
	p.lock.RLock()
	started := p.downloadStarted
	p.lock.RUnlock()
	c.True(started.After(time.Now().Add(-maxWaitForChunkDownload)), "the stall clock must restart when the request goes out")

	// And with the request made, the peer answers for it: an expired deadline now costs it the piece and the connection
	one.lock.Lock()
	one.timeout = time.Now().Add(-time.Second)
	one.lock.Unlock()
	p.clearExpiredDownloads()
	p.lock.RLock()
	held = len(p.pieces)
	bailing = p.bail
	p.lock.RUnlock()
	c.Equal(0, held, "a piece the peer was asked for and never delivered must be given up")
	c.True(bailing, "a peer that was asked for a piece and delivered nothing must be dropped")
}

// TestTheDeadlineStartsWhenTheRequestReachesTheWire verifies that the peer's own write goroutine is what starts the
// deadline for a piece, since it is the only thing that knows when a request finally went out.
func TestTheDeadlineStartsWhenTheRequestReachesTheWire(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p, _ := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	c.NoError(conn.SetDeadline(time.Now().Add(peerMgmtWait)))

	// The peer unchokes us and we ask it for a piece. The deadline is recorded as the request is handed to the socket,
	// so it has been started by the time the request itself arrives here.
	_, err := conn.Write(newTestMessage(unchokeID))
	c.NoError(err)
	waitForMessagesToBeProcessed(t, conn, p)
	p.queuePieceDownload(0)
	buffer := make([]byte, requestMessageLength)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestPieceRequest(requestID, 0, 0, chunkSize), buffer)

	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	c.NotNil(one)
	one.lock.RLock()
	requested := one.requested
	deadline := one.timeout
	one.lock.RUnlock()
	c.True(requested, "the request going out must start the deadline for the piece")
	c.True(deadline.After(time.Now()), "the deadline for the piece must be pushed out to when the request went out")
}

// TestAChunkArrivingAfterAChokeKeepsTheGrace verifies that a chunk landing just after the peer chokes us doesn't pull
// the choke's 60 second grace back in to the ordinary 10 second deadline. Requests are pipelined, so a chunk asked for
// before a choke routinely arrives just after it, and shortening the deadline there throws away exactly the partial
// piece that suspending the download exists to protect.
func TestAChunkArrivingAfterAChokeKeepsTheGrace(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	// Pieces of more than one chunk, so that a chunk can arrive without completing the piece
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.queuePieceDownload(0)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	c.NotNil(one)

	// The peer chokes us mid-piece, which holds the piece for the full choked wait
	p.lock.Lock()
	p.peerChoking = true
	p.lock.Unlock()
	p.suspendDownloads()
	one.lock.RLock()
	suspended := one.timeout
	one.lock.RUnlock()
	c.True(suspended.After(time.Now().Add(downloadReadDeadline)), "choking must push the deadline for the piece out")

	// A chunk from before the choke lands, which must not shorten what the choke granted
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize)))
	one.lock.RLock()
	afterChunk := one.timeout
	one.lock.RUnlock()
	c.Equal(suspended, afterChunk, "a chunk arriving during a choke must not pull the deadline back in")
	p.clearExpiredDownloads()
	p.lock.RLock()
	held := len(p.pieces)
	p.lock.RUnlock()
	c.Equal(1, held, "the partial piece the choke was granted time for must be kept")
}

// TestAChunkForAPieceWeAreNotDownloadingIsIgnored verifies that a duplicate delivery doesn't cost the peer its
// connection. We cause them ourselves: the requests of a batch made before a choke go out after the unchoke that
// follows, alongside the fresh batch resuming builds, so an honest peer serves the same chunk twice and the second
// copy arrives once the first has completed the piece and taken it out of our hands.
func TestAChunkForAPieceWeAreNotDownloadingIsIgnored(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// Nothing is claimed, so every chunk sent is one we aren't downloading
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize)),
		"a chunk for a piece we aren't downloading must not end the peer session")
	c.False(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()),
		"a peer must not be blocked for a duplicate delivery we asked for ourselves")

	// The same over the wire leaves the connection alone
	payload := make([]byte, 8, 8+chunkSize)
	payload = append(payload, testStorageBytes(0, 0, chunkSize)...)
	_, err := conn.Write(newTestMessage(pieceID, payload...))
	c.NoError(err)
	select {
	case <-done:
		t.Fatal("the peer was dropped over a chunk for a piece we aren't downloading")
	case <-time.After(250 * time.Millisecond):
	}
}

// TestProtocolViolationsCloseTheConnection verifies that a peer that has broken the protocol has its connection closed
// there and then, rather than only having its address blocked. Blocking alone leaves the socket to the read loop's
// teardown, which first waits for a slot on the write queue: with piece messages draining under an upload cap, a peer
// we've just decided is hostile goes on holding a peer slot, and receiving what we queued for it, for tens of seconds.
func TestProtocolViolationsCloseTheConnection(t *testing.T) {
	for _, one := range []struct {
		name    string
		message []byte
		claim   bool
	}{
		{name: "an oversized message", message: []byte{0xFF, 0xFF, 0xFF, 0xFF}},
		{name: "a message length that doesn't match its ID", message: newTestMessage(haveID)},
		{name: "a bit field of the wrong length", message: newTestMessage(bitFieldID, 0, 0)},
		{name: "a request for data that doesn't exist", message: newTestPieceRequest(requestID, testPieceCount, 0, 1)},
		{
			name:    "a chunk we never asked for",
			claim:   true,
			message: newTestMessage(pieceID, append(make([]byte, 8, 8+chunkSize/2), make([]byte, chunkSize/2)...)...),
		},
		{
			name:    "a piece that doesn't hash to what the torrent says",
			claim:   true,
			message: newTestMessage(pieceID, append(make([]byte, 8, 8+chunkSize), make([]byte, chunkSize)...)...),
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			// A dispatcher of its own, since blocking the address would otherwise carry into the other cases
			d := newTestDispatcher(t)
			client := newTestClient(d)
			conn, p := newTestPeer(t, client)
			defer xio.CloseIgnoringErrors(conn)
			if one.claim {
				p.queuePieceDownload(0)
			}

			// Everything we owe the peer is queued up and the cap has it draining a message at a time, which is the
			// state that leaves the teardown unable to close the connection for tens of seconds
			client.OutRate.SetCap(2 * chunkSize)
			for range cap(p.writeQueue) {
				p.writeQueue <- make([]byte, 13+chunkSize)
			}
			go p.processIncomingMessages()

			_, err := conn.Write(one.message)
			c.NoError(err)
			waitForConnClose(t, conn, peerMgmtWait)
			c.True(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()))
		})
	}
}

// waitForConnClose reads whatever the peer has to say until it closes the connection, failing the test if it doesn't
// do so within the allowed time. Reading is what tells a closed connection apart from one that merely has data on it,
// which a peer with a backed up write queue always does.
func waitForConnClose(t *testing.T, conn net.Conn, within time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	for {
		if _, err := conn.Read(buffer); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("the connection to the peer was not closed")
			}
			return
		}
	}
}

// TestOutstandingPieceRequestsAreDroppedAtTeardown verifies that the requests still in hand when a peer session ends
// are thrown away rather than fulfilled. Nothing can be delivered at that point — the responses are discarded by the
// write queue's drain or fail on a dead connection — so building them turns the 13KB of requests a peer sent, which
// may well be what got it disconnected for flooding us, into some 8MB of storage reads for nothing.
func TestOutstandingPieceRequestsAreDroppedAtTeardown(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	client.file = newTestStorage(t, client)
	markTestPiecesAvailable(client, 0, 1, 2, 3)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.setChoked(false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.pieceRequestQueue()
	}()

	// Fill the write queue, so that the responses have nowhere to go and the requests pile up behind them
	for range cap(p.writeQueue) {
		p.writeQueue <- make([]byte, 4)
	}
	const requests = maxPendingPieceRequests / 2
	for i := range requests {
		p.requestChan <- &pieceRequest{index: i % testPieceCount, length: chunkSize}
	}

	// The session ends, which is what closing the channel feeding the requests in stands for
	close(p.requestChan)
	select {
	case <-done:
	case <-time.After(peerMgmtWait):
		t.Fatal("the goroutine holding the outstanding piece requests was left running")
	}
	served := drainPieceMessages(t, p)
	c.True(served <= 1, "%d of the %d requests still in hand at teardown were read from storage", served, requests)
}

// markTestRequestsSent records that the requests for the pieces the peer holds have gone out on the wire, which is
// what starts the clocks deciding whether it is still delivering them. A peer whose write goroutine is running does
// this for itself as each request is handed to the socket; one a test built directly has to be told.
func markTestRequestsSent(t *testing.T, p *peer) {
	t.Helper()
	p.lock.RLock()
	pieces := maps.Clone(p.pieces)
	p.lock.RUnlock()
	if len(pieces) == 0 {
		t.Fatal("the peer holds no pieces, so nothing could have been asked for")
	}
	for _, one := range pieces {
		one.lock.Lock()
		one.requested = true
		one.lock.Unlock()
	}
}

// downloadDeadline returns the deadline for the given piece along with the last time the peer made progress.
func downloadDeadline(p *peer, one *piece) (deadline, lastReceived time.Time) {
	one.lock.RLock()
	deadline = one.timeout
	one.lock.RUnlock()
	p.lock.RLock()
	lastReceived = p.lastReceived
	p.lock.RUnlock()
	return deadline, lastReceived
}

// TestAStorageWriteFailureStopsTheTorrent verifies that a piece that can't be written to storage isn't simply
// downloaded all over again. A full disk or a file we have no permission to write doesn't heal, so re-selecting the
// piece pulls it off the network again and again at whatever rate the swarm can manage, marks no progress, and says
// nothing about any of it beyond another log line each time around.
func TestAStorageWriteFailureStopsTheTorrent(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithHashes())
	defer client.closeRateLimiters()
	client.file = newReadOnlyTestStorage(t, client)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The peer has the whole torrent and isn't choking us, so it has somewhere to go next if the failure to store what
	// it just delivered isn't treated as final
	p.lock.Lock()
	p.peerChoking = false
	p.has = everyTestPiece()
	p.lock.Unlock()
	c.Equal(0, client.tracker.selectForDownloading(p, everyTestPiece()))
	p.queuePieceDownload(0)
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize)))

	c.HasError(client.fatalError(), "a piece that can't be written must stop the torrent")
	c.False(client.tracker.hasPiece(0), "a piece that can't be written must not be counted as one we have")
	p.lock.RLock()
	claimed := slices.Sorted(maps.Keys(p.pieces))
	p.lock.RUnlock()
	c.Equal(0, len(claimed), "the piece must not be claimed again, indexes: %v", claimed)
	client.tracker.lock.RLock()
	who := client.tracker.who[0]
	client.tracker.lock.RUnlock()
	c.Nil(who)
}

// newReadOnlyTestStorage creates the storage file for the test torrent and hands back a handle that can only be read
// from, so that writing a piece to it fails the way a full disk or a file we have no permission to write would.
func newReadOnlyTestStorage(t *testing.T, client *Client) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test"+tfs.DownloadExt)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(make([]byte, client.torrentFile.Size())); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if f, err = os.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xio.CloseIgnoringErrors(f) })
	return f
}

// TestChunkOutsideThePieceIsRejected verifies that a chunk whose range doesn't fit inside the piece it claims to be
// part of is refused. The offset is unverified data from the peer, so on a platform where int is 32 bits a large
// enough one arrives as a negative number, which would slice the buffer out of bounds and panic.
func TestChunkOutsideThePieceIsRejected(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	// Pieces of more than one chunk, so that a chunk which fits within the piece can still be one we never asked for
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	pieceLength := int(client.torrentFile.Info.PieceLength)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.queuePieceDownload(0)

	for _, one := range []struct {
		name   string
		begin  int
		length int
	}{
		{name: "a negative offset", begin: -1, length: 16},
		{name: "an offset that wrapped around", begin: math.MinInt32, length: 16},
		{name: "an offset just past the end", begin: pieceLength, length: 1},
		{name: "a chunk that runs past the end", begin: pieceLength - 8, length: 16},
		{name: "a chunk larger than the piece", begin: 0, length: pieceLength + 1},
	} {
		c.HasError(p.receivedChunk(0, one.begin, make([]byte, one.length)), one.name)
	}

	// The piece is still there, untouched, and a chunk that does fit is accepted
	c.NoError(p.receivedChunk(0, pieceLength-chunkSize, testStorageBytes(0, pieceLength-chunkSize, chunkSize)))
}

// TestChunksWeNeverAskedForAreRefused verifies that a chunk which isn't one of the chunk aligned blocks we asked the
// peer for is refused, even though it fits within the piece. Taking whatever range a peer cares to send is what lets
// it hold a piece without ever delivering anything usable: any byte we haven't seen before counts as progress, so a
// single new byte every few seconds renews both of the deadlines that decide whether the peer is still working on the
// piece, and no other peer can take it while it is claimed. It is also what makes taking a chunk in cost more than the
// chunk is worth, since each fragment that doesn't join up with its neighbors becomes another span of the piece to be
// searched and shifted through on every one that follows.
func TestChunksWeNeverAskedForAreRefused(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.queuePieceDownload(0)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	c.NotNil(one)
	deadline, received := downloadDeadline(p, one)

	for _, test := range []struct {
		name   string
		begin  int
		length int
	}{
		{name: "a single byte", begin: 0, length: 1},
		{name: "a single byte partway in", begin: chunkSize + 1, length: 1},
		{name: "part of a chunk", begin: 0, length: chunkSize / 2},
		{name: "a chunk at an unaligned offset", begin: 1, length: chunkSize},
		{name: "a chunk straddling two of ours", begin: chunkSize / 2, length: chunkSize},
		{name: "more than a chunk", begin: 0, length: chunkSize + 1},
		{name: "two chunks at once", begin: 0, length: 2 * chunkSize},
		{name: "no data at all", begin: 0, length: 0},
	} {
		c.HasError(p.receivedChunk(0, test.begin, testStorageBytes(0, test.begin, test.length)), test.name)
	}

	// None of it counted as progress, was recorded as part of the piece, or renewed anything
	one.lock.RLock()
	spans := len(one.spans.Spans)
	one.lock.RUnlock()
	c.Equal(0, spans, "nothing we didn't ask for may be recorded as part of the piece")
	stillDeadline, stillReceived := downloadDeadline(p, one)
	c.Equal(deadline, stillDeadline, "a chunk we never asked for must not renew the deadline for the piece")
	c.Equal(received, stillReceived, "a chunk we never asked for must not count as progress for the peer")
	c.True(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()),
		"the peer must be blocked for sending what it was never asked for")

	// A chunk we did ask for is still taken in
	c.NoError(p.receivedChunk(0, chunkSize, testStorageBytes(0, chunkSize, chunkSize)))
	one.lock.RLock()
	spans = len(one.spans.Spans)
	one.lock.RUnlock()
	c.Equal(1, spans)
}

// TestTheShortFinalChunkOfAPieceIsAccepted verifies that the last chunk of a piece that isn't a whole number of chunks
// long is both asked for and accepted at the length it actually has. Requiring a full chunk everywhere would leave the
// tail of such a torrent impossible to complete.
func TestTheShortFinalChunkOfAPieceIsAccepted(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	const tail = 100
	torrentFile := newTestTorrentFileWithChunksPerPiece(4, testPieceCount)
	torrentFile.Info.Length = torrentFile.Info.PieceLength*(testPieceCount-1) + chunkSize + tail
	client := newTestClientForTorrent(d, torrentFile)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	startChunkRequestDelivery(t, p)

	last := testPieceCount - 1
	c.Equal(int64(chunkSize+tail), client.torrentFile.LengthOf(last))
	p.queuePieceDownload(last)

	// What we ask for is a full chunk followed by whatever is left of the piece
	c.Equal(newTestPieceRequest(requestID, uint32(last), 0, chunkSize), nextQueuedMessage(t, p))
	c.Equal(newTestPieceRequest(requestID, uint32(last), chunkSize, tail), nextQueuedMessage(t, p))

	// And the short chunk is what is accepted at that offset. Only that one is delivered, so the piece doesn't
	// complete and nothing but the length rule is being exercised.
	c.HasError(p.receivedChunk(last, chunkSize, testStorageBytes(last, chunkSize, tail-1)),
		"a chunk shorter than what is left of the piece was never asked for")
	c.NoError(p.receivedChunk(last, chunkSize, testStorageBytes(last, chunkSize, tail)))
}

// TestQueuingAPieceDoesNotBlockOnABackedUpPeer verifies that claiming a piece from a peer doesn't wait for the
// messages asking for its chunks to fit onto that peer's write queue. A piece is a whole queue's worth of requests and
// the queue drains no faster than the peer and the rate limiter allow, which is tens of seconds for a slow peer with
// piece messages already queued, while the callers that claim a piece are the peer management goroutine and the read
// goroutines of other peers: waiting there stops choke rotation, expiry clearing and dialing dead for as long as the
// slowest peer takes.
func TestQueuingAPieceDoesNotBlockOnABackedUpPeer(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	startChunkRequestDelivery(t, p)

	// Nothing more will fit on the write queue, which is what a peer whose messages are draining slowly looks like
	for range cap(p.writeQueue) {
		p.writeQueue <- newStateMessage(chokeID)
	}

	waitFor(t, "queuePieceDownload", func() { p.queuePieceDownload(0) })
	p.lock.RLock()
	_, claimed := p.pieces[0]
	p.lock.RUnlock()
	c.True(claimed, "the piece must be claimed even though nothing could be asked for yet")

	// The requests are waiting on the delivery goroutine rather than lost, so they go out as the queue drains
	for range cap(p.writeQueue) {
		<-p.writeQueue
	}
	for i := range 4 {
		c.Equal(newTestPieceRequest(requestID, 0, uint32(i*chunkSize), chunkSize), nextQueuedMessage(t, p), "chunk %d", i)
	}
}

// TestChunkRequestsAreBuiltWhenTheyGoOut verifies that the messages asking for a piece's chunks are put together as
// they are about to be sent rather than when the piece was claimed, so a chunk that arrives while the queue ahead of
// them is draining isn't asked for all over again. Asking again spends the peer's upload bandwidth, and our download
// bandwidth, on data we already hold.
func TestChunkRequestsAreBuiltWhenTheyGoOut(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithChunksPerPiece(4, testPieceCount))
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The piece is claimed and the first two chunks arrive before any of the requests have gone out, which is what not
	// having started the delivery goroutine yet stands in for
	p.queuePieceDownload(0)
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize)))
	c.NoError(p.receivedChunk(0, chunkSize, testStorageBytes(0, chunkSize, chunkSize)))

	startChunkRequestDelivery(t, p)
	for i := 2; i < 4; i++ {
		c.Equal(newTestPieceRequest(requestID, 0, uint32(i*chunkSize), chunkSize), nextQueuedMessage(t, p), "chunk %d", i)
	}
	select {
	case buffer := <-p.writeQueue:
		t.Fatalf("only the chunks that hadn't arrived should have been asked for, but %v was too", buffer)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestMessageDeadlinesScaleWithTheMessageLength verifies that how long a message is given to move, in either
// direction, depends on how big it is. Everything but the bit field has a small, fixed size, but a bit field is one
// byte for every eight pieces of the torrent, so a flat deadline for one of those amounts to demanding a transfer rate
// that a slow but honest peer has no way to meet.
func TestMessageDeadlinesScaleWithTheMessageLength(t *testing.T) {
	c := check.New(t)

	// Everything that fits within the minimum still gets the minimum
	for _, length := range []uint32{0, 1, 5, 13, 9 + chunkSize, uint32(msgReadDeadline/time.Second) * minMessageRate} {
		c.Equal(msgReadDeadline, messageReadDeadline(length), "length %d", length)
	}
	for _, length := range []int{0, 4, 5, 17, 13 + chunkSize, int(msgWriteDeadline/time.Second) * minMessageRate} {
		c.Equal(msgWriteDeadline, messageWriteDeadline(length), "length %d", length)
	}

	// The bit field for the largest torrent that will be accepted is a quarter of a megabyte, which the flat deadline
	// gave a peer five seconds to deliver: ~52KB/s or be dropped, and blocked from reconnecting on top of it
	maxBitField := 1 + tfs.MaxPieceCount/8
	c.Equal(time.Duration(maxBitField)*time.Second/minMessageRate, messageReadDeadline(uint32(maxBitField)))
	c.Equal(time.Duration(4+maxBitField)*time.Second/minMessageRate, messageWriteDeadline(4+maxBitField))
	for _, one := range []struct {
		name     string
		deadline time.Duration
	}{
		{name: "read", deadline: messageReadDeadline(uint32(maxBitField))},
		{name: "write", deadline: messageWriteDeadline(4 + maxBitField)},
	} {
		c.True(one.deadline >= 30*time.Second,
			"the largest bit field must be given enough time for a slow peer to %s it: %v", one.name, one.deadline)
	}
}

// TestALargeBitFieldIsGivenTimeToArrive verifies that the deadline actually placed on the connection while a message
// body is read is the one derived from its length, and that a peer sending a large bit field is taken in rather than
// dropped. A timeout costs more than the connection: it is neither EOF nor a connection we closed, so the address is
// blocked as well and the peer can't come back.
func TestALargeBitFieldIsGivenTimeToArrive(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithPieces(bigBitFieldPieceCount))
	local, recorder, p, done := startTestPeerRecordingDeadlines(t, client)

	bits := make([]byte, bigBitFieldPieceCount/8)
	bits[0] = 0x80 // The peer has the first piece
	c.NoError(local.SetDeadline(time.Now().Add(peerMgmtWait)))
	_, err := local.Write(newTestMessage(bitFieldID, bits...))
	c.NoError(err)

	deadline := time.Now().Add(peerMgmtWait)
	for {
		p.lock.RLock()
		took := p.has.IsSet(0)
		p.lock.RUnlock()
		if took {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bit field was never taken in")
		}
		time.Sleep(time.Millisecond)
	}
	// The upper bound is what tells the deadline for the body apart from the far longer idle one the read loop waits
	// for the next message under, which would otherwise satisfy a test that only asked for more than the minimum. The
	// deadline for the body is the second one armed, since the four byte length prefix is read under the idle one.
	expected := messageReadDeadline(uint32(1 + len(bits)))
	allowance := recorder.allowance(t, "read", 1)
	c.True(allowance > msgReadDeadline && allowance <= expected,
		"the body of a large bit field must be read under a deadline of %v, not %v", expected, allowance)
	c.False(d.GateKeeper().IsAddressBlocked(p.conn.RemoteAddr()),
		"a peer that delivered its bit field must not be blocked")

	xio.CloseIgnoringErrors(local)
	select {
	case <-done:
	case <-time.After(peerMgmtWait):
		t.Fatal("the peer kept processing messages after the connection ended")
	}
}

// TestALargeBitFieldIsGivenTimeToGoOut verifies that the deadline placed on the connection while a message is written
// is derived from its length too. A peer that reads slowly is no more at fault than one that writes slowly, and the
// bit field we send the moment a peer connects is as big as the one it sends us, so a flat deadline drops the peers
// that need us most: the ones on the far end of a slow link, which is exactly when a seed matters.
func TestALargeBitFieldIsGivenTimeToGoOut(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClientForTorrent(d, newTestTorrentFileWithPieces(bigBitFieldPieceCount))
	// We have a piece, so the bit field announcing it is the first thing the peer is sent
	client.tracker.lock.Lock()
	client.tracker.have.Set(0)
	client.tracker.lock.Unlock()
	local, recorder, _, done := startTestPeerRecordingDeadlines(t, client)

	bits := make([]byte, bigBitFieldPieceCount/8)
	bits[0] = 0x80
	expectBitField(t, local, bits...)

	expected := messageWriteDeadline(len(newBitFieldMessage(bits)))
	allowance := recorder.allowance(t, "write", 0)
	c.True(allowance > msgWriteDeadline && allowance <= expected,
		"a large bit field must be written under a deadline of %v, not %v", expected, allowance)

	xio.CloseIgnoringErrors(local)
	select {
	case <-done:
	case <-time.After(peerMgmtWait):
		t.Fatal("the peer kept processing messages after the connection ended")
	}
}

// bigBitFieldPieceCount is a piece count large enough that the bit field for it is worth more than the flat deadlines
// at minMessageRate, which is what the tests that check those deadlines scale need.
const bigBitFieldPieceCount = 400000

// startTestPeerRecordingDeadlines starts a peer whose connection records the deadlines placed on it, and returns the
// connection the remote would use to talk to it, the recorder, the peer, and a channel that is closed once the peer
// stops processing messages.
func startTestPeerRecordingDeadlines(t *testing.T, client *Client) (local net.Conn, recorder *deadlineRecordingConn,
	p *peer, done chan struct{},
) {
	t.Helper()
	local, remote := newTestConnPair(t)
	recorder = &deadlineRecordingConn{Conn: remote}
	p = newPeer(client, recorder, client.logger)
	client.lock.Lock()
	client.peers[recorder] = p
	client.lock.Unlock()
	done = make(chan struct{})
	go func() {
		defer close(done)
		p.processIncomingMessages()
	}()
	return local, recorder, p, done
}

// deadlineRecordingConn records how long each deadline placed on a connection allowed, so that a test can tell how
// long a peer was given for what it was in the middle of sending or being sent.
type deadlineRecordingConn struct {
	net.Conn
	allowances map[string][]time.Duration
	lock       sync.Mutex
}

func (c *deadlineRecordingConn) SetReadDeadline(when time.Time) error {
	c.record("read", when)
	return c.Conn.SetReadDeadline(when)
}

func (c *deadlineRecordingConn) SetWriteDeadline(when time.Time) error {
	c.record("write", when)
	return c.Conn.SetWriteDeadline(when)
}

func (c *deadlineRecordingConn) record(kind string, when time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.allowances == nil {
		c.allowances = make(map[string][]time.Duration)
	}
	c.allowances[kind] = append(c.allowances[kind], time.Until(when))
}

// allowance returns how long the nth deadline of the given kind allowed, failing the test if that many were never set.
func (c *deadlineRecordingConn) allowance(t *testing.T, kind string, n int) time.Duration {
	t.Helper()
	c.lock.Lock()
	defer c.lock.Unlock()
	if len(c.allowances[kind]) <= n {
		t.Fatalf("the peer set %d %s deadlines, so there is no #%d", len(c.allowances[kind]), kind, n)
	}
	return c.allowances[kind][n]
}

// TestChokeDoesNotCostTheConnectionOrThePiece verifies that a peer choking us in the middle of a piece, which real
// clients do as a matter of course when they rotate who they upload to, neither drops the connection nor throws away
// what has already arrived, and that the requests it discarded are made again once it unchokes us.
func TestChokeDoesNotCostTheConnectionOrThePiece(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The peer unchokes us and we ask it for a piece
	c.NoError(conn.SetDeadline(time.Now().Add(peerMgmtWait)))
	_, err := conn.Write(newTestMessage(unchokeID))
	c.NoError(err)
	p.queuePieceDownload(0)
	buffer := make([]byte, 17)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestPieceRequest(requestID, 0, 0, chunkSize), buffer)

	// It chokes us before sending anything, which must push the deadline for the piece out rather than let it run out
	_, err = conn.Write(newTestMessage(chokeID))
	c.NoError(err)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	waitForDeadlineBeyond(t, one, time.Now().Add(downloadReadDeadline))

	// Nothing gives up on a peer that is choking us, so the piece and the connection both survive
	p.clearExpiredDownloads()
	p.lock.RLock()
	held := len(p.pieces)
	p.lock.RUnlock()
	c.Equal(1, held, "the piece must be kept while the peer is choking us")
	select {
	case <-done:
		t.Fatal("the connection was dropped for a peer that only choked us")
	default:
	}

	// Once it unchokes us, the request it was told to discard has to be made again
	_, err = conn.Write(newTestMessage(unchokeID))
	c.NoError(err)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestPieceRequest(requestID, 0, 0, chunkSize), buffer)
}

// TestRepeatedChokesDoNotExtendTheClaim verifies that only an actual transition into being choked pushes the deadline
// for a piece we're in the middle of downloading out. Acting on every choke message instead would let a remote that
// claimed a piece and then re-sent choke often enough keep it for as long as it liked: the piece could never expire,
// no other peer could take it while it was claimed, and a peer that is choking us is excused from the stall check, so
// nothing would drop the connection either.
func TestRepeatedChokesDoNotExtendTheClaim(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p, _ := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	c.NoError(conn.SetDeadline(time.Now().Add(peerMgmtWait)))

	// The peer unchokes us, we ask it for a piece, and it chokes us before sending any of it. The unchoke is waited for
	// rather than merely sent, since resuming what we hold is part of taking it in and the piece must not be claimed
	// while that is still under way.
	_, err := conn.Write(newTestMessage(unchokeID))
	c.NoError(err)
	waitForMessagesToBeProcessed(t, conn, p)
	p.queuePieceDownload(0)
	buffer := make([]byte, 17)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestPieceRequest(requestID, 0, 0, chunkSize), buffer)
	_, err = conn.Write(newTestMessage(chokeID))
	c.NoError(err)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	waitForDeadlineBeyond(t, one, time.Now().Add(downloadReadDeadline))
	one.lock.RLock()
	suspended := one.timeout
	one.lock.RUnlock()

	// A second choke, with nothing in between, must leave the deadline exactly where the first one put it
	_, err = conn.Write(newTestMessage(chokeID))
	c.NoError(err)
	waitForMessagesToBeProcessed(t, conn, p)
	one.lock.RLock()
	repeated := one.timeout
	one.lock.RUnlock()
	c.Equal(suspended, repeated, "a repeated choke must not push the deadline for the piece out again")
}

// TestRepeatedUnchokesDoNotResendRequests verifies that only an actual transition out of being choked resumes the
// downloads we hold. Acting on every unchoke message instead would have a remote that re-sends it restart the deadline
// for each piece it holds, restart the clock the stall check measures from, and make us send the whole outstanding set
// of requests again — seventeen bytes for every chunk still missing, in answer to the five it cost to ask.
func TestRepeatedUnchokesDoNotResendRequests(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p, _ := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	c.NoError(conn.SetDeadline(time.Now().Add(peerMgmtWait)))

	// The peer unchokes us and we ask it for a piece, which it never answers. The unchoke is waited for rather than
	// merely sent, since resuming what we hold is part of taking it in and the piece must not be claimed while that is
	// still under way.
	_, err := conn.Write(newTestMessage(unchokeID))
	c.NoError(err)
	waitForMessagesToBeProcessed(t, conn, p)
	p.queuePieceDownload(0)
	buffer := make([]byte, 17)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestPieceRequest(requestID, 0, 0, chunkSize), buffer)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	one.lock.RLock()
	deadline := one.timeout
	one.lock.RUnlock()

	// A second unchoke, with nothing in between, must neither restart the deadline nor ask for the chunk again
	_, err = conn.Write(newTestMessage(unchokeID))
	c.NoError(err)
	waitForMessagesToBeProcessed(t, conn, p)
	one.lock.RLock()
	repeated := one.timeout
	one.lock.RUnlock()
	c.Equal(deadline, repeated, "a repeated unchoke must not restart the deadline for the piece")
	for _, message := range remainingMessages(t, conn) {
		c.False(len(message) != 0 && message[0] == requestID, "a repeated unchoke must not ask for the chunk again")
	}
}

// TestAlternatingChokesCannotHoldAPieceForever verifies that the deadline juggling a peer choking and unchoking us
// causes is bounded by when the piece last saw data. Pushing the deadline out for a choke and restarting it for an
// unchoke are both right on their own, so without that bound a remote that alternates the two never has to deliver
// anything at all, and no other peer can take the piece while it doesn't.
func TestAlternatingChokesCannotHoldAPieceForever(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.queuePieceDownload(0)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()

	// The deadline has been kept ahead of the clock, but nothing has arrived for the piece since it was claimed
	one.lock.Lock()
	one.timeout = time.Now().Add(maxChokedDownloadWait)
	one.lastProgress = time.Now().Add(-maxDownloadStall - time.Second)
	one.lock.Unlock()
	p.clearExpiredDownloads()
	p.lock.RLock()
	held := len(p.pieces)
	p.lock.RUnlock()
	c.Equal(0, held, "a piece that has gone maxDownloadStall without data must be given up whatever its deadline says")

	// A piece that is still being delivered is left alone, however long ago it was claimed
	p.queuePieceDownload(1)
	p.lock.RLock()
	one = p.pieces[1]
	p.lock.RUnlock()
	one.lock.Lock()
	one.timeout = time.Now().Add(downloadReadDeadline)
	one.lastProgress = time.Now().Add(-maxDownloadStall + peerMgmtWait)
	one.lock.Unlock()
	p.clearExpiredDownloads()
	p.lock.RLock()
	held = len(p.pieces)
	p.lock.RUnlock()
	c.Equal(1, held, "a piece that is still being delivered must be kept")
}

// waitForMessagesToBeProcessed sends the peer a message flipping whether the remote says it is interested in what we
// have and returns once that has been taken in. Messages are processed in order, so this is what says that everything
// sent before it has been acted on as well, without a sleep whose length decides whether the test is testing anything.
// The remote's interest is what gets flipped because taking it in has no other effect on the peer.
func waitForMessagesToBeProcessed(t *testing.T, conn net.Conn, p *peer) {
	t.Helper()
	p.lock.RLock()
	expected := !p.peerInterested
	p.lock.RUnlock()
	id := notInterestedID
	if expected {
		id = interestedID
	}
	if _, err := conn.Write(newTestMessage(id)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(peerMgmtWait)
	for {
		p.lock.RLock()
		actual := p.peerInterested
		p.lock.RUnlock()
		if actual == expected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer never processed the messages sent to it")
		}
		time.Sleep(time.Millisecond)
	}
}

// remainingMessages returns the messages the peer sends within a short window, so that a test can assert that a
// particular one isn't among them.
func remainingMessages(t *testing.T, conn net.Conn) [][]byte {
	t.Helper()
	var messages [][]byte
	lengthBuffer := make([]byte, 4)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, lengthBuffer); err != nil {
			return messages
		}
		buffer := make([]byte, binary.BigEndian.Uint32(lengthBuffer))
		if _, err := io.ReadFull(conn, buffer); err != nil {
			return messages
		}
		messages = append(messages, buffer)
	}
}

// waitForDeadlineBeyond fails the test if the piece's deadline isn't pushed out past the given time.
func waitForDeadlineBeyond(t *testing.T, one *piece, beyond time.Time) {
	t.Helper()
	deadline := time.Now().Add(peerMgmtWait)
	for {
		one.lock.RLock()
		timeout := one.timeout
		one.lock.RUnlock()
		if timeout.After(beyond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the deadline for the piece was never pushed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestResumeOnlyAsksForTheChunksThatAreMissing verifies that the requests made again after a peer unchokes us cover
// just the chunks that haven't arrived, rather than asking for the whole piece over again.
func TestResumeOnlyAsksForTheChunksThatAreMissing(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	startChunkRequestDelivery(t, p)

	// A piece three chunks long, of which the first and last have already arrived
	one := &piece{buffer: make([]byte, 3*chunkSize), timeout: time.Now().Add(-time.Minute)}
	one.spans.Insert(&spanlist.Span{Start: 0, Length: chunkSize})
	one.spans.Insert(&spanlist.Span{Start: 2 * chunkSize, Length: chunkSize})
	p.lock.Lock()
	p.pieces[2] = one
	p.lock.Unlock()

	p.resumeDownloads()
	// Read with a deadline rather than a bare receive: asking for nothing at all is one of the ways resuming can go
	// wrong, and that must fail this test rather than park it on the queue forever and hang the whole suite
	c.Equal(newTestPieceRequest(requestID, 2, chunkSize, chunkSize), nextQueuedMessage(t, p))
	select {
	case buffer := <-p.writeQueue:
		t.Fatalf("only the missing chunk should have been asked for, but %v was too", buffer)
	default:
	}
	one.lock.RLock()
	timeout := one.timeout
	one.lock.RUnlock()
	c.True(timeout.After(time.Now()), "the deadline for the piece must be restarted")
}

// TestKeepAliveIsAnchoredToTheLastWrite verifies that the wait for the next keep-alive runs from the last thing we
// put on the wire rather than from the previous attempt at one. A keep-alive that finds a write has just happened is
// dropped as unnecessary, so measuring from the attempt leaves the next one a full period further out and lets the
// wire go quiet for nearly 2×keepAlivePeriod. That is past the 2m30s this library itself waits for a message before
// dropping the connection — and blocking the address for five minutes, since a read timeout is neither a peer hanging
// up nor a connection we closed — and past the two minutes typical clients allow.
func TestKeepAliveIsAnchoredToTheLastWrite(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	conn, p := newTestPeer(t, newTestClient(d))
	defer xio.CloseIgnoringErrors(conn)

	// A connection that has yet to send anything counts as quiet since it was made, so its first keep-alive is a full
	// period out
	c.Equal(p.created, p.lastWrite())
	c.Equal(keepAlivePeriod, p.keepAliveDelay(p.created))

	now := time.Now()

	for _, quiet := range []time.Duration{
		0, time.Second, keepAlivePeriod / 2, keepAlivePeriod - time.Millisecond, keepAlivePeriod, 2 * keepAlivePeriod,
	} {
		p.setLastWrite(now.Add(-quiet))
		expected := max(keepAlivePeriod-quiet, 0)
		c.Equal(expected, p.keepAliveDelay(now), "quiet for %v", quiet)
	}

	// The case the anchoring is for: a message goes out just after a keep-alive did, so the keep-alive coming due at
	// the end of that period is dropped as unnecessary. What follows has to be due a period after that message — a
	// second from now here — rather than a period after the attempt that was dropped.
	p.setLastWrite(now.Add(time.Second - keepAlivePeriod))
	c.True(now.Sub(p.lastWrite()) < keepAlivePeriod, "the keep-alive coming due now would be dropped as unnecessary")
	c.Equal(time.Second, p.keepAliveDelay(now))
}

// TestReadDeadlines verifies that we're willing to wait longer than the keep-alive period for the next message from a
// peer, since a peer with nothing to say would otherwise be disconnected and blocked, while the remainder of a message
// that has started to arrive is still expected promptly.
func TestReadDeadlines(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, remote := newTestConnPair(t)
	defer xio.CloseIgnoringErrors(conn)
	recorder := &deadlineConn{Conn: remote, deadlines: make(chan time.Duration, 8)}
	p := newPeer(client, recorder, client.logger)
	go p.processIncomingMessages()

	idle := nextReadDeadline(t, recorder)
	c.True(idle > keepAlivePeriod, "waiting for a message must outlast the keep-alive period of %v, was %v",
		keepAlivePeriod, idle)

	// Send just the length prefix of a message, so that the peer starts waiting for the rest of it
	_, err := conn.Write(newTestMessage(haveID, 0, 0, 0, 0)[:4])
	c.NoError(err)
	rest := nextReadDeadline(t, recorder)
	c.True(rest <= msgReadDeadline, "waiting for the rest of a message must be no more than %v, was %v",
		msgReadDeadline, rest)
}

// TestOversizedMessageIsRejected verifies that a peer can't demand an arbitrarily large allocation from us.
func TestOversizedMessageIsRejected(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)
	limit := newTestPeerMessageLimit(t, client)
	for _, one := range []struct {
		name   string
		length uint32
	}{
		{name: "one byte too large", length: limit + 1},
		{name: "as large as the length prefix allows", length: math.MaxUint32},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			conn, _, done := startTestPeer(t, client)
			defer xio.CloseIgnoringErrors(conn)
			buffer := make([]byte, 4)
			binary.BigEndian.PutUint32(buffer, one.length)
			_, werr := conn.Write(buffer)
			c.NoError(werr)
			// The rejection must happen without waiting for a message body that will never arrive
			select {
			case <-done:
			case <-time.After(msgReadDeadline / 2):
				t.Fatal("peer did not reject the oversized message")
			}
		})
	}
}

// TestMessageLimitIsSizedForTheTorrent verifies that the limit tracks the torrent rather than being a single figure
// large enough for the biggest torrent allowed. A peer on a torrent of a few pieces has no legitimate reason to send
// a message larger than a piece message, and must not be able to make us allocate as if it did.
func TestMessageLimitIsSizedForTheTorrent(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)

	// A small torrent is held to the size of a piece message, the largest of the fixed-size messages
	c.Equal(uint32(dispatcher.MaxPieceMessageLength-4), newTestPeerMessageLimit(t, newTestClient(d)))

	// A torrent whose bit field is larger than that has the room its bit field actually needs, and no more
	const pieceCount = 140000
	client := newTestClientForTorrent(d, newTestTorrentFileWithPieces(pieceCount))
	defer client.closeRateLimiters()
	c.Equal(uint32(1+pieceCount/8), newTestPeerMessageLimit(t, client))

	// The largest torrent that can be loaded still has a limit that is bounded and known
	client = newTestClientForTorrent(d, newTestTorrentFileWithPieces(tfs.MaxPieceCount))
	defer client.closeRateLimiters()
	c.Equal(uint32(1+tfs.MaxPieceCount/8), newTestPeerMessageLimit(t, client))
	c.Equal(uint32(262145), newTestPeerMessageLimit(t, client))
}

// newTestPeerMessageLimit returns the largest message length a peer of the given client will accept.
func newTestPeerMessageLimit(t *testing.T, client *Client) uint32 {
	t.Helper()
	_, p := newTestPeer(t, client)
	return p.maxMessageLength()
}

// TestLargestAllowedMessageIsAccepted verifies that the message size limit isn't off by one.
func TestLargestAllowedMessageIsAccepted(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	limit := newTestPeerMessageLimit(t, client)
	conn, _, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// An unknown message ID is ignored, no matter how large it is, as long as it is within the limit
	_, err := conn.Write(newTestMessage(200, make([]byte, limit-1)...))
	c.NoError(err)
	select {
	case <-done:
		t.Fatal("peer rejected a message that was within the size limit")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestInvalidPieceRequestsAreRejected verifies that a peer can't ask us for data that doesn't exist, which would
// otherwise result in an allocation sized by the peer and a read beyond the end of the piece.
func TestInvalidPieceRequestsAreRejected(t *testing.T) {
	d := newTestDispatcher(t)
	for _, one := range []struct {
		name   string
		index  uint32
		begin  uint32
		length uint32
	}{
		{name: "no length", length: 0},
		{name: "one byte more than a chunk", length: chunkSize + 1},
		{name: "as large as the length field allows", length: math.MaxUint32},
		{name: "index beyond the last piece", index: testPieceCount, length: chunkSize},
		{name: "absurd index", index: math.MaxUint32, length: chunkSize},
		{name: "range extends beyond the end of the piece", begin: chunkSize / 2, length: chunkSize},
		{name: "offset beyond the end of the piece", begin: chunkSize, length: 1},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			conn, _, done := startTestPeer(t, newTestClient(d))
			defer xio.CloseIgnoringErrors(conn)
			_, werr := conn.Write(newTestPieceRequest(requestID, one.index, one.begin, one.length))
			c.NoError(werr)
			select {
			case <-done:
			case <-time.After(msgReadDeadline / 2):
				t.Fatal("peer did not reject the invalid piece request")
			}
		})
	}
}

// TestValidPieceRequestIsServed verifies that the piece request validation doesn't reject legitimate requests.
func TestValidPieceRequestIsServed(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	client.file = newTestStorage(t, client)
	markTestPiecesAvailable(client, 1)
	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	expectBitField(t, conn, 0x40)

	// Let the peer know it isn't choked, so that it will serve requests
	p.setChoked(false)
	c.NoError(conn.SetReadDeadline(time.Now().Add(msgReadDeadline)))
	buffer := make([]byte, 5)
	_, err := io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestMessage(unchokeID), buffer)

	// Ask for the second half of the second piece
	const begin = chunkSize / 2
	const length = chunkSize / 2
	_, err = conn.Write(newTestPieceRequest(requestID, 1, begin, length))
	c.NoError(err)
	buffer = make([]byte, 13+length)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(uint32(9+length), binary.BigEndian.Uint32(buffer[:4]))
	c.Equal(pieceID, buffer[4])
	c.Equal(uint32(1), binary.BigEndian.Uint32(buffer[5:9]))
	c.Equal(uint32(begin), binary.BigEndian.Uint32(buffer[9:13]))
	c.Equal(testStorageBytes(1, begin, length), buffer[13:])

	select {
	case <-done:
		t.Fatal("peer closed the connection for a valid piece request")
	default:
	}
}

// TestPieceRequestForPieceWeDontHaveIsIgnored verifies that a request for a piece we haven't downloaded yet is never
// answered, since the storage holds nothing but whatever it was initialized with for that range and the remote would
// treat the response as a corrupt piece and ban us.
func TestPieceRequestForPieceWeDontHaveIsIgnored(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	client.file = newTestStorage(t, client)

	// We have the second piece, but not the first
	markTestPiecesAvailable(client, 1)
	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	expectBitField(t, conn, 0x40)

	// Let the peer know it isn't choked, so that it will serve requests
	p.setChoked(false)
	c.NoError(conn.SetReadDeadline(time.Now().Add(msgReadDeadline)))
	buffer := make([]byte, 5)
	_, err := io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(newTestMessage(unchokeID), buffer)

	// Ask for the piece we don't have, then the one we do. Requests are served in the order they arrive, so the
	// response to the second request coming back first proves the first was never served.
	_, err = conn.Write(newTestPieceRequest(requestID, 0, 0, chunkSize))
	c.NoError(err)
	_, err = conn.Write(newTestPieceRequest(requestID, 1, 0, chunkSize))
	c.NoError(err)

	buffer = make([]byte, dispatcher.MaxPieceMessageLength)
	_, err = io.ReadFull(conn, buffer)
	c.NoError(err)
	c.Equal(uint32(9+chunkSize), binary.BigEndian.Uint32(buffer[:4]))
	c.Equal(pieceID, buffer[4])
	c.Equal(uint32(1), binary.BigEndian.Uint32(buffer[5:9]), "the piece we don't have must not have been served")
	c.Equal(uint32(0), binary.BigEndian.Uint32(buffer[9:13]))
	c.Equal(testStorageBytes(1, 0, chunkSize), buffer[13:])

	select {
	case <-done:
		t.Fatal("peer closed the connection for a valid piece request")
	default:
	}
}

// TestChokingDiscardsQueuedPieceRequests verifies that the requests a peer made before we choked it are thrown away
// rather than served once the queue in front of them clears. BEP 3 has the choked side treat everything it asked for
// as discarded, so the up to 512 requests we're willing to hold — some 8MB of piece data — would otherwise be
// uploaded to a peer we had just choked, and counted as wasted by the remote when it arrived. Shedding that upload is
// the entire point of choking it.
func TestChokingDiscardsQueuedPieceRequests(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	client.file = newTestStorage(t, client)
	markTestPiecesAvailable(client, 0, 1, 2, 3)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.setChoked(false)
	startTestPieceRequestQueue(t, p)

	// Fill the write queue, so that the responses have nowhere to go and the requests pile up behind them. That is
	// both the state that has us choking a peer in the first place and the one in which its requests are still there
	// to be served later. The count stays well short of the flood limit, which would disconnect the peer for an
	// entirely different reason.
	for range cap(p.writeQueue) {
		p.writeQueue <- make([]byte, 4)
	}
	const requests = maxPendingPieceRequests / 2
	for i := range requests {
		p.requestChan <- &pieceRequest{index: i % testPieceCount, length: chunkSize}
	}
	p.setChoked(true)
	waitForChokeToBeHandled(t, p)

	// Unchoking must not bring back the requests the peer was told to treat as discarded: the only response that may
	// still arrive is the one already prepared when the write queue filled up
	p.setChoked(false)
	served := drainPieceMessages(t, p)
	c.True(served <= 1, "%d of the %d requests made before choking were served", served, requests)

	// A request that arrives while we're choking isn't served either, however it got to the queue
	p.setChoked(true)
	p.requestChan <- &pieceRequest{index: 0, length: chunkSize}
	c.Equal(0, drainPieceMessages(t, p), "a request must not be served while we're choking the peer")
}

// startTestPieceRequestQueue starts the goroutine holding the peer's outstanding piece requests and stops it once the
// test is over. It only stops when the channel feeding it is closed, which is normally done by the teardown of the
// read loop, and nothing runs that here: leaving it open leaks both this goroutine and the one it spawns to fulfill
// the requests, for the remainder of the run.
func startTestPieceRequestQueue(t *testing.T, p *peer) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.pieceRequestQueue()
	}()
	t.Cleanup(func() {
		close(p.requestChan)
		select {
		case <-done:
		case <-time.After(peerMgmtWait):
			t.Error("the goroutine holding the outstanding piece requests was left running")
		}
	})
}

// waitForChokeToBeHandled returns once the goroutine holding the peer's outstanding piece requests has finished acting
// on the choke that was signaled to it. The signal channel holds a single entry, so a second signal can only be
// accepted once the first has been taken, and can itself only be taken once the first has been acted on: waiting for
// both to be consumed is therefore enough to know the requests have been discarded. Sleeping instead would let a
// loaded scheduler run an unchoke first, leaving the queue intact and the test passing or failing at random.
func waitForChokeToBeHandled(t *testing.T, p *peer) {
	t.Helper()
	select {
	case p.chokedChan <- struct{}{}:
	case <-time.After(peerMgmtWait):
		t.Fatal("the choke was never taken up")
	}
	deadline := time.Now().Add(peerMgmtWait)
	for len(p.chokedChan) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the choke was never acted on")
		}
		time.Sleep(time.Millisecond)
	}
}

// drainPieceMessages returns the number of piece messages the peer has queued up for writing, once none have arrived
// for long enough to say that no more are coming.
func drainPieceMessages(t *testing.T, p *peer) int {
	t.Helper()
	count := 0
	for {
		select {
		case buffer := <-p.writeQueue:
			if len(buffer) > 4 && buffer[4] == pieceID {
				count++
			}
		case <-time.After(250 * time.Millisecond):
			return count
		}
	}
}

// TestPieceRequestFloodIsRejected verifies that a peer that asks for far more than we can deliver is disconnected
// rather than allowed to grow the pending request queue without bound.
func TestPieceRequestFloodIsRejected(t *testing.T) {
	d := newTestDispatcher(t)
	client := newTestClient(d)
	client.file = newTestStorage(t, client)
	markTestPiecesAvailable(client, 0, 1, 2, 3)
	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	expectBitField(t, conn, 0xF0)

	// Throttle what may be written before unchoking. Without this, the responses drain out of the pending queue and
	// into the kernel's socket buffers as fast as the requests arrive, so whether the queue ever reaches its limit
	// depends on how much the machine happens to buffer for a loopback connection. The cap has to leave room for a
	// whole piece message, since the rate limiter refuses any single amount larger than the cap outright.
	client.OutRate.SetCap(2 * chunkSize)
	p.setChoked(false)

	// Ask for more than will be held onto, without ever reading the responses
	for i := range maxPendingPieceRequests + 128 {
		if _, err := conn.Write(newTestPieceRequest(requestID, uint32(i%testPieceCount), 0, chunkSize)); err != nil {
			break
		}
	}

	// The disconnect must come from the flood detection, which happens immediately, rather than from the write
	// deadline expiring while the responses back up
	select {
	case <-done:
	case <-time.After(msgWriteDeadline / 2):
		t.Fatal("peer did not reject the piece request flood")
	}
}

// TestUpdateInterestIsSerialized verifies that concurrent interest updates, which arrive from both the goroutine
// reading messages from a peer and the one managing peers, yield a single consistent sequence of interested and
// not-interested messages. Each message must flip our stated interest, and the last one must agree with what we
// recorded, or the remote's view of our interest ends up permanently at odds with ours.
func TestUpdateInterestIsSerialized(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// Run the goroutine that would normally be delivering the peer's state for it
	done := make(chan bool)
	defer close(done)
	go p.processStateChanges(done)

	// Flip whether the peer has anything we want from several goroutines at once, each following its change with the
	// interest update that the message handlers and peer management would make
	const goroutines = 8
	const iterations = 250
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Go(func() {
			for j := range iterations {
				p.lock.Lock()
				if (i+j)%2 == 0 {
					p.has.Set(0)
				} else {
					p.has.Unset(0)
				}
				p.lock.Unlock()
				p.updateInterest()
			}
		})
	}
	wg.Wait()

	// Collect the message IDs the peer would have written, rather than sending them over the connection. Nothing was
	// draining the queue while the flips were being made, so the delivery only finishes once we start.
	sent := collectStateMessages(t, p)
	c.True(len(sent) > 0, "no interest was ever expressed")
	for i, buffer := range sent {
		// We start out uninterested, so the messages must alternate, beginning with an interested one
		expected := interestedID
		if i%2 == 1 {
			expected = notInterestedID
		}
		if buffer[4] != expected {
			t.Fatalf("message %d was %d, not %d", i, buffer[4], expected)
		}
	}
	p.lock.RLock()
	amInterested := p.amInterested
	p.lock.RUnlock()
	c.Equal(amInterested, sent[len(sent)-1][4] == interestedID, "the last message must match the interest we recorded")
}

// TestUpdateInterestDoesNotRecordAStaleDecision verifies that an interest update can't overwrite what one that looked
// at the peer more recently decided. The decision is made from a snapshot and recorded afterwards, and both the
// goroutine reading messages from the peer and the one managing peers make it, so an update that started earlier
// finishing later would leave our stated interest contradicting what the peer has told us it has, with nothing to
// correct it until the next peer adjustment 15 seconds later.
func TestUpdateInterestDoesNotRecordAStaleDecision(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// Deciding whether a peer has anything we want needs the tracker's lock, so holding it parks an update that has
	// already taken its snapshot of the peer, which at this point holds nothing of interest to us
	client.tracker.lock.Lock()
	first := make(chan struct{})
	go func() { defer close(first); p.updateInterest() }()

	// A second update starts while the first is still parked. The waits are only here to give each goroutine time to
	// reach the point it will block at; whether they are long enough decides whether a stale decision could be
	// recorded, not whether a fresh one is.
	time.Sleep(50 * time.Millisecond)
	second := make(chan struct{})
	go func() { defer close(second); p.updateInterest() }()
	time.Sleep(50 * time.Millisecond)

	// The peer tells us it has a piece we need, which only an update that has yet to look at the peer can take in
	p.lock.Lock()
	p.has.Set(0)
	p.lock.Unlock()
	client.tracker.lock.Unlock()

	<-first
	<-second
	p.lock.RLock()
	amInterested := p.amInterested
	p.lock.RUnlock()
	c.True(amInterested, "the peer has a piece we need, so the interest recorded last must say we want it")
}

// collectStateMessages returns the messages the peer's state delivery goroutine has queued up, waiting until what the
// peer has been told about our interest has caught up with what was recorded. Only safe to call once the goroutines
// making state changes have finished, since it relies on the interest settling.
func collectStateMessages(t *testing.T, p *peer) [][]byte {
	t.Helper()
	var sent [][]byte
	deadline := time.Now().Add(peerMgmtWait)
	for {
		p.lock.RLock()
		interested := p.amInterested
		caughtUp := p.toldInterested == interested
		p.lock.RUnlock()
		select {
		case buffer := <-p.writeQueue:
			sent = append(sent, buffer)
			continue
		default:
		}
		// Nothing can still be in flight once the peer has been told what we recorded, the queue is empty and the last
		// message we collected says what we recorded, since every message in between would have to be one of those
		if caughtUp && len(sent) != 0 && (sent[len(sent)-1][4] == interestedID) == interested {
			return sent
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer was never told about the interest that was recorded")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStateChangesDoNotBlockOnAFullWriteQueue verifies that a peer whose write queue has backed up can't hold up the
// goroutines that only need to record something for it to be told about. Peer management walks every peer in turn and
// a piece that finishes validating is announced to every peer from the read goroutine of the one that completed it, so
// a single peer that can't be written to must not be able to stall the handling of all the others.
func TestStateChangesDoNotBlockOnAFullWriteQueue(t *testing.T) {
	d := newTestDispatcher(t)
	for _, one := range []struct {
		change   func(client *Client, p *peer)
		name     string
		expected []byte
	}{
		{
			name: "interest",
			change: func(_ *Client, p *peer) {
				p.lock.Lock()
				p.has.Set(0)
				p.lock.Unlock()
				p.updateInterest()
			},
			expected: newStateMessage(interestedID),
		},
		{
			name:     "choking",
			change:   func(_ *Client, p *peer) { p.setChoked(false) },
			expected: newStateMessage(unchokeID),
		},
		{
			name:     "have",
			change:   func(client *Client, _ *peer) { client.informPeersWeHavePiece(2) },
			expected: newHaveMessage(2),
		},
		{
			name:     "peer adjustment",
			change:   func(client *Client, _ *peer) { client.adjustPeers() },
			expected: newStateMessage(unchokeID),
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			client := newTestClient(d)
			conn, p := newTestPeer(t, client)
			defer xio.CloseIgnoringErrors(conn)

			// Fill the write queue, so that anything else sent to it has nowhere to go
			for range cap(p.writeQueue) {
				p.writeQueue <- make([]byte, 4)
			}
			waitFor(t, one.name, func() { one.change(client, p) })

			// The message must still be delivered, once the queue has room for it again
			done := make(chan bool)
			defer close(done)
			go p.processStateChanges(done)
			for range cap(p.writeQueue) {
				c.Equal(4, len(nextQueuedMessage(t, p)))
			}
			c.Equal(one.expected, nextQueuedMessage(t, p))
		})
	}
}

// nextQueuedMessage returns the next message the peer has queued up for writing, failing the test if none arrives.
func nextQueuedMessage(t *testing.T, p *peer) []byte {
	t.Helper()
	select {
	case buffer := <-p.writeQueue:
		return buffer
	case <-time.After(peerMgmtWait):
		t.Fatal("no message was queued up for the peer")
		return nil
	}
}

// TestPieceIsNotClaimedByAPeerThatIsOnItsWayOut verifies that a peer whose teardown has already run doesn't end up
// holding a piece. Nothing would ever come back for it, so the piece would be marked as being downloaded by a peer
// that no longer exists and no other peer could take it for as long as the torrent ran.
func TestPieceIsNotClaimedByAPeerThatIsOnItsWayOut(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The peer claims a piece just as its own teardown finishes
	c.Equal(0, client.tracker.selectForDownloading(p, everyTestPiece()))
	p.lock.Lock()
	p.bail = true
	p.lock.Unlock()
	p.queuePieceDownload(0)

	p.lock.RLock()
	held := len(p.pieces)
	p.lock.RUnlock()
	c.Equal(0, held, "a peer that is bailing out must not take on a piece")
	client.tracker.lock.RLock()
	downloading := client.tracker.downloading.IsSet(0)
	who := client.tracker.who[0]
	client.tracker.lock.RUnlock()
	c.False(downloading, "the piece must be left available for another peer")
	c.Nil(who)
}

// TestExpiredDownloadsLeaveAnotherPeersClaimAlone verifies that a peer giving up a piece can't take the claim away
// from the peer that has since taken it up, which would leave two peers downloading the same piece and one of them
// unable to ever finish it.
func TestExpiredDownloadsLeaveAnotherPeersClaimAlone(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	otherConn, other := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(otherConn)

	// The first peer still has an expired download recorded for a piece the second peer has since claimed
	p.lock.Lock()
	p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Minute), requested: true}
	p.lock.Unlock()
	c.Equal(0, client.tracker.selectForDownloading(other, everyTestPiece()))

	p.clearExpiredDownloads()
	client.tracker.lock.RLock()
	downloading := client.tracker.downloading.IsSet(0)
	who := client.tracker.who[0]
	client.tracker.lock.RUnlock()
	c.True(downloading, "the claim of the peer that is downloading the piece must be left alone")
	c.Equal(other, who)

	// Its own record of the piece is still given up, and the claim goes away once its owner releases it
	p.lock.RLock()
	held := len(p.pieces)
	p.lock.RUnlock()
	c.Equal(0, held)
	client.tracker.clearDownload(0, other)
	client.tracker.lock.RLock()
	downloading = client.tracker.downloading.IsSet(0)
	client.tracker.lock.RUnlock()
	c.False(downloading)
}

// TestAFreedPieceNudgesTheOtherPeers verifies that a peer giving up an expired claim tells the rest of them to look
// for something to download. Nothing else does: an already connected, unchoked and idle peer is never revisited by
// peer management or by an interest update, so in a small or static swarm the freed piece would otherwise sit
// unclaimed until the peer that let it expire happened to unchoke us or some peer disconnected.
func TestAFreedPieceNudgesTheOtherPeers(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	otherConn, other := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(otherConn)

	// The first peer is choking us and has let its claim expire, so the piece is given up but the connection is kept
	c.Equal(0, client.tracker.selectForDownloading(p, everyTestPiece()))
	p.lock.Lock()
	p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Minute), requested: true}
	p.lock.Unlock()

	// The second peer has the whole torrent and isn't choking us, but has nothing to do
	other.lock.Lock()
	other.peerChoking = false
	other.has = everyTestPiece()
	other.lock.Unlock()

	p.clearExpiredDownloads()

	p.lock.RLock()
	bailing := p.bail
	p.lock.RUnlock()
	c.False(bailing, "a peer that is choking us must not be dropped for failing to deliver")
	other.lock.RLock()
	claimed := slices.Sorted(maps.Keys(other.pieces))
	other.lock.RUnlock()
	c.Equal([]int{0}, claimed, "the freed piece must be taken up by a peer that can download it")
	client.tracker.lock.RLock()
	who := client.tracker.who[0]
	client.tracker.lock.RUnlock()
	c.Equal(other, who)
}

// TestInterestCoversPiecesAnotherPeerHasClaimed verifies that our interest in a peer is decided by the pieces we lack
// rather than by the ones that are free to claim at that moment. Near the end of a download every piece we're missing
// can be claimed at once, and telling our peers we want nothing then costs us exactly the connections we still need:
// typical remotes choke a peer that says it wants nothing, our own rotation ranks an uninterested peer first to drop,
// and getting one back takes an interested/unchoke round trip, or a reconnect, every time a claim frees up.
func TestInterestCoversPiecesAnotherPeerHasClaimed(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	otherConn, other := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(otherConn)

	// We have all but the last piece of the torrent, and the other peer has claimed that one
	markTestPiecesAvailable(client, 0, 1, 2)
	p.lock.Lock()
	p.has = everyTestPiece()
	p.lock.Unlock()
	c.Equal(testPieceCount-1, client.tracker.selectForDownloading(other, everyTestPiece()))

	c.True(p.updateInterest().amInterested, "a peer holding the piece we still need is of interest, claimed or not")

	// Once we have that piece too, there is nothing left to want from it
	markTestPiecesAvailable(client, testPieceCount-1)
	c.False(p.updateInterest().amInterested, "a peer with nothing we lack must not be of interest")
}

// everyTestPiece returns a bit set claiming every piece of the test torrent, as a peer that has the whole thing would.
func everyTestPiece() *fixedbits.Bits {
	has := fixedbits.New(testPieceCount)
	for i := range testPieceCount {
		has.Set(i)
	}
	return has
}

// TestGivingUpOnAPeerClosesTheConnection verifies that a peer we've given up on is disconnected right away. The read
// loop only looks at the flag between messages and can be parked waiting for the next one for minutes, which would
// leave a peer we're done with holding one of the limited peer slots for all of that time.
func TestGivingUpOnAPeerClosesTheConnection(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, remote := newTestConnPair(t)
	recorder := &deadlineConn{Conn: remote, deadlines: make(chan time.Duration, 8)}
	p := newPeer(client, recorder, client.logger)
	client.lock.Lock()
	client.peers[recorder] = p
	client.lock.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.processIncomingMessages()
	}()

	// Wait until the peer is parked waiting for the next message, which is past the point where it last looked at the
	// flag saying we're done with it
	c.True(nextReadDeadline(t, recorder) > keepAlivePeriod)

	// The peer was asked for a piece and never delivered any of it
	p.lock.Lock()
	p.peerChoking = false
	p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Second), requested: true}
	p.lock.Unlock()
	p.clearExpiredDownloads()

	select {
	case <-done:
	case <-time.After(peerMgmtWait):
		t.Fatal("the peer we gave up on was left connected")
	}
	checkConnOpen(t, conn, false)
}

// TestRoutineDisconnectsDoNotBlockTheAddress verifies that an address is only blocked for something the peer did
// wrong. Banning a well-behaved peer for hanging up, or for a connection we closed ourselves during the ordinary
// churn of rotating peers, keeps us from talking to it for minutes and can leave a small swarm with nothing to
// connect to.
func TestRoutineDisconnectsDoNotBlockTheAddress(t *testing.T) {
	for _, one := range []struct {
		disconnect func(t *testing.T, local net.Conn, p *peer)
		name       string
		blocked    bool
	}{
		{
			name:       "the remote hangs up",
			disconnect: func(_ *testing.T, local net.Conn, _ *peer) { xio.CloseIgnoringErrors(local) },
		},
		{
			name:       "we close the connection ourselves",
			disconnect: func(_ *testing.T, _ net.Conn, p *peer) { xio.CloseIgnoringErrors(p.conn) },
		},
		{
			name:       "we give up on the peer",
			disconnect: func(_ *testing.T, _ net.Conn, p *peer) { p.bailOut() },
		},
		{
			name:    "the peer sends something malformed",
			blocked: true,
			disconnect: func(t *testing.T, local net.Conn, _ *peer) {
				t.Helper()
				if _, err := local.Write(newTestMessage(haveID)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			// A dispatcher of its own, since every one of these connections comes from the same address
			d := newTestDispatcher(t)
			conn, p, done := startTestPeer(t, newTestClient(d))
			defer xio.CloseIgnoringErrors(conn)
			addr := p.conn.RemoteAddr()

			one.disconnect(t, conn, p)
			select {
			case <-done:
			case <-time.After(peerMgmtWait):
				t.Fatal("the peer kept processing messages after the connection ended")
			}
			c.Equal(one.blocked, d.GateKeeper().IsAddressBlocked(addr))
		})
	}
}

func TestPendingRequests(t *testing.T) {
	c := check.New(t)
	q := newPendingRequests()
	c.Equal(0, q.count())
	c.Nil(q.next())

	// Requests are fulfilled in the order they were received
	for i := range 3 {
		c.True(q.add(&pieceRequest{index: i, length: chunkSize}))
	}
	c.Equal(3, q.count())
	for i := range 3 {
		req := q.next()
		c.NotNil(req)
		c.Equal(i, req.index)
		q.removeNext()
	}
	c.Nil(q.next())
	c.Equal(0, q.count())

	// A cancellation removes the request it matches and leaves the others in order
	for i := range 3 {
		c.True(q.add(&pieceRequest{index: i, length: chunkSize}))
	}
	c.True(q.add(&pieceRequest{index: 1, length: chunkSize, cancel: true}))
	c.Equal(2, q.count())
	c.Equal(0, q.next().index)
	q.removeNext()
	c.Equal(2, q.next().index)
	q.removeNext()
	c.Nil(q.next())

	// A cancellation that matches nothing is harmless
	c.True(q.add(&pieceRequest{index: 22, length: chunkSize, cancel: true}))
	c.Equal(0, q.count())

	// Only so many requests will be held onto
	q = newPendingRequests()
	for i := range maxPendingPieceRequests {
		c.True(q.add(&pieceRequest{index: i % testPieceCount, begin: i, length: chunkSize}), "request %d", i)
	}
	c.False(q.add(&pieceRequest{index: 0, begin: maxPendingPieceRequests, length: chunkSize}))
	c.Equal(maxPendingPieceRequests, q.count())

	// Room becomes available again as requests are fulfilled
	c.NotNil(q.next())
	q.removeNext()
	c.Equal(maxPendingPieceRequests-1, q.count())
	c.True(q.add(&pieceRequest{index: 0, begin: maxPendingPieceRequests, length: chunkSize}))
	c.Equal(maxPendingPieceRequests, q.count())
}

// deadlineConn records the read deadlines that are set on a connection.
type deadlineConn struct {
	net.Conn
	deadlines chan time.Duration
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	select {
	case c.deadlines <- time.Until(t):
	default:
	}
	return c.Conn.SetReadDeadline(t)
}

// nextReadDeadline returns how long the peer is willing to wait for the data it is currently reading.
func nextReadDeadline(t *testing.T, conn *deadlineConn) time.Duration {
	t.Helper()
	select {
	case deadline := <-conn.deadlines:
		return deadline
	case <-time.After(msgReadDeadline):
		t.Fatal("no read deadline was set")
		return 0
	}
}

// markTestPiecesAvailable marks the given pieces of the test torrent as ones we have, so that requests for them will
// be served. Intended to be called before any peers have been added, since it deliberately bypasses the notification
// that would otherwise be sent to them.
func markTestPiecesAvailable(client *Client, indexes ...int) {
	client.tracker.lock.Lock()
	defer client.tracker.lock.Unlock()
	for _, index := range indexes {
		client.tracker.have.Set(index)
	}
}

// newTestStorage creates the storage file for the test torrent, filled with a pattern that identifies each offset.
func newTestStorage(t *testing.T, client *Client) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "test"+tfs.DownloadExt))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xio.CloseIgnoringErrors(f) })
	if _, err = f.Write(testStorageBytes(0, 0, int(client.torrentFile.Size()))); err != nil {
		t.Fatal(err)
	}
	return f
}

// testStorageBytes returns the data the test storage holds for a range within a piece.
func testStorageBytes(index, begin, length int) []byte {
	buffer := make([]byte, length)
	for i := range buffer {
		buffer[i] = byte((index*chunkSize + begin + i) % 251)
	}
	return buffer
}

// newTestPieceRequest creates a request or cancel message for a range within a piece.
func newTestPieceRequest(id byte, index, begin, length uint32) []byte {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	binary.BigEndian.PutUint32(payload[8:], length)
	return newTestMessage(id, payload...)
}

// newTestMessage creates a peer message with the given ID and payload, prefixed with its length.
func newTestMessage(id byte, payload ...byte) []byte {
	buffer := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buffer[:4], uint32(1+len(payload)))
	buffer[4] = id
	copy(buffer[5:], payload)
	return buffer
}

// startTestPeer adds a peer to the client and starts it processing incoming messages from the returned connection.
// The returned channel is closed when that peer stops processing them.
func startTestPeer(t *testing.T, client *Client) (conn net.Conn, p *peer, done chan struct{}) {
	t.Helper()
	conn, p = newTestPeer(t, client)
	done = make(chan struct{})
	go func() {
		defer close(done)
		p.processIncomingMessages()
	}()
	return conn, p, done
}

// startChunkRequestDelivery starts the goroutine that puts the messages asking for the chunks we still need onto the
// peer's write queue, and stops it once the test is over. A peer whose read loop is running starts it for itself; one
// a test built directly has to have it started, since without it the requests a piece is queued for never go out.
func startChunkRequestDelivery(t *testing.T, p *peer) {
	t.Helper()
	done := make(chan bool)
	go p.processChunkRequests(done)
	t.Cleanup(func() { close(done) })
}

// newTestPeer adds a peer to the client and returns the connection the remote side of that peer would use to talk to
// it, along with the peer itself.
func newTestPeer(t *testing.T, client *Client) (conn net.Conn, p *peer) {
	t.Helper()
	conn, remote := newTestConnPair(t)
	p = newPeer(client, remote, client.logger)
	client.lock.Lock()
	client.peers[remote] = p
	client.lock.Unlock()
	return conn, p
}

// newTestPeerFromHost adds a peer to the client the way newTestPeer does, but has it report the given address as the
// remote end of its connection. Every connection a test makes is on the loopback interface, which leaves all of the
// peers built from them looking like the same host to peer management, so a test that needs its peers to be on hosts
// of their own uses this instead.
func newTestPeerFromHost(t *testing.T, client *Client, host string, port int) (conn net.Conn, p *peer) {
	t.Helper()
	conn, remote := newTestConnPair(t)
	fixed := &fixedRemoteAddrConn{Conn: remote, remoteAddr: &net.TCPAddr{IP: net.ParseIP(host), Port: port}}
	p = newPeer(client, fixed, client.logger)
	client.lock.Lock()
	client.peers[fixed] = p
	client.lock.Unlock()
	return conn, p
}

// fixedRemoteAddrConn reports the remote address it was given rather than the loopback address the connection it wraps
// was actually made to.
type fixedRemoteAddrConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (c *fixedRemoteAddrConn) RemoteAddr() net.Addr { return c.remoteAddr }

// newTestConnPair returns the two ends of a loopback connection.
func newTestConnPair(t *testing.T) (local, remote net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer xio.CloseIgnoringErrors(listener)
	accepted := make(chan net.Conn, 1)
	go func() {
		one, aerr := listener.Accept()
		if aerr != nil {
			close(accepted)
			return
		}
		accepted <- one
	}()
	if local, err = net.Dial("tcp", listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	remote, ok := <-accepted
	if !ok {
		t.Fatal("unable to accept the connection")
	}
	t.Cleanup(func() {
		xio.CloseIgnoringErrors(local)
		xio.CloseIgnoringErrors(remote)
	})
	return local, remote
}

// newTestDispatcher creates a dispatcher for a test and stops it once the test is over. The external IP lookup is
// always replaced with a fixed address, since the real one is started by the dispatcher's listen goroutine as soon as
// the dispatcher exists: leaving it in place makes the test depend on outside sites being reachable and leaves a probe
// goroutine issuing HTTP requests to them for as long as those take to give up, which outlives the test.
func newTestDispatcher(t *testing.T) *dispatcher.Dispatcher {
	t.Helper()
	d, err := dispatcher.NewDispatcher(dispatcher.FixedExternalIP(net.ParseIP(testExternalIP)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d
}

func newTestClient(d *dispatcher.Dispatcher) *Client {
	return newTestClientForTorrent(d, newTestTorrentFile())
}

func newTestClientForTorrent(d *dispatcher.Dispatcher, torrentFile *tfs.File) *Client {
	c := &Client{
		InRate:              d.InRate.New(math.MaxInt32),
		OutRate:             d.OutRate.New(math.MaxInt32),
		dispatcher:          d,
		torrentFile:         torrentFile,
		logger:              slog.New(slog.DiscardHandler),
		peerWaitGroup:       &sync.WaitGroup{},
		peerMgmtStop:        make(chan struct{}),
		peers:               make(map[net.Conn]*peer),
		dialing:             make(map[string]bool),
		stoppedChan:         make(chan bool, 1),
		concurrentDownloads: 4,
		peersWanted:         32,
	}
	c.tracker = newTracker(c)
	return c
}

func newTestTorrentFile() *tfs.File {
	return newTestTorrentFileWithPieces(testPieceCount)
}

// newTestTorrentFileWithHashes creates the test torrent with the piece hashes that match what testStorageBytes holds,
// so that a piece assembled from it actually validates.
func newTestTorrentFileWithHashes() *tfs.File {
	f := newTestTorrentFile()
	for i := range testPieceCount {
		sum := sha1.Sum(testStorageBytes(i, 0, chunkSize)) //nolint:gosec // The spec requires sha1
		copy(f.Info.Pieces[i*sha1Size:], sum[:])
	}
	return f
}

func newTestTorrentFileWithPieces(count int) *tfs.File {
	var f tfs.File
	f.Info.Name = "test"
	f.Info.PieceLength = chunkSize
	f.Info.Pieces = make([]byte, sha1Size*count)
	f.Info.Length = chunkSize * int64(count)
	return &f
}

// newTestTorrentFileWithChunksPerPiece creates the test torrent with pieces made up of the given number of chunks, for
// the tests that need a piece to be able to arrive a chunk at a time. The single chunk pieces the rest of them use
// can't: a chunk has to be one of the blocks we asked for, and the only such block those pieces have is the whole
// piece.
func newTestTorrentFileWithChunksPerPiece(chunks, count int) *tfs.File {
	f := newTestTorrentFileWithPieces(count)
	f.Info.PieceLength = int64(chunks) * chunkSize
	f.Info.Length = f.Info.PieceLength * int64(count)
	return f
}
