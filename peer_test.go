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
	d, err := dispatcher.NewDispatcher()
	check.New(t).NoError(err)
	defer d.Stop()
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	conn, _, done := startTestPeer(t, newTestClient(d))
	defer xio.CloseIgnoringErrors(conn)

	// Tell the peer we have piece 0, which should result in it telling us it is interested
	_, err = conn.Write(newTestMessage(haveID, 0, 0, 0, 0))
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

// TestBitFieldSpareBitsAreIgnored verifies that a peer that sets the spare bits at the end of its bit field, which
// correspond to pieces that don't exist, can't get us to request a piece index beyond the end of the torrent. Doing so
// would panic when the piece it returned was validated against the piece hashes.
func TestBitFieldSpareBitsAreIgnored(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	_, err = conn.Write(newTestMessage(bitFieldID, 0xFF))
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
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	conn, _, _ := startTestPeer(t, newTestClient(d))
	defer xio.CloseIgnoringErrors(conn)

	// Tell the peer we have a piece, which makes it interested. That message arriving first proves no bit field came
	// ahead of it.
	_, err = conn.Write(newTestMessage(haveID, 0, 0, 0, 0))
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	p.queuePieceDownload(0)
	p.lock.RLock()
	one := p.pieces[0]
	p.lock.RUnlock()
	c.NotNil(one)

	// The first half of the piece arrives, which is progress
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize/2)))
	deadline, received := downloadDeadline(p, one)

	// The same half arrives again, carrying nothing new
	c.NoError(p.receivedChunk(0, 0, testStorageBytes(0, 0, chunkSize/2)))
	repeated, repeatedReceived := downloadDeadline(p, one)
	c.Equal(deadline, repeated, "a duplicate chunk must not renew the deadline for the piece")
	c.Equal(received, repeatedReceived, "a duplicate chunk must not count as progress for the peer")

	// A chunk holding data we don't have yet does renew it
	c.NoError(p.receivedChunk(0, chunkSize/2, testStorageBytes(0, chunkSize/2, chunkSize/4)))
	renewed, renewedReceived := downloadDeadline(p, one)
	c.True(renewed.After(deadline), "a chunk with new data must renew the deadline for the piece")
	c.True(renewedReceived.After(received), "a chunk with new data must count as progress for the peer")

	// With the deadline no longer being renewed, the piece is given up and the peer dropped
	one.lock.Lock()
	one.timeout = time.Now().Add(-time.Second)
	one.lock.Unlock()
	p.clearExpiredDownloads()
	p.lock.RLock()
	remaining := len(p.pieces)
	p.lock.RUnlock()
	c.Equal(0, remaining, "the piece must be given up once its deadline passes")
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

// TestChunkOutsideThePieceIsRejected verifies that a chunk whose range doesn't fit inside the piece it claims to be
// part of is refused. The offset is unverified data from the peer, so on a platform where int is 32 bits a large
// enough one arrives as a negative number, which would slice the buffer out of bounds and panic.
func TestChunkOutsideThePieceIsRejected(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
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
		{name: "an offset just past the end", begin: chunkSize, length: 1},
		{name: "a chunk that runs past the end", begin: chunkSize - 8, length: 16},
		{name: "a chunk larger than the piece", begin: 0, length: chunkSize + 1},
	} {
		c.HasError(p.receivedChunk(0, one.begin, make([]byte, one.length)), one.name)
	}

	// The piece is still there, untouched, and a chunk that does fit is accepted
	c.NoError(p.receivedChunk(0, chunkSize-16, testStorageBytes(0, chunkSize-16, 16)))
}

// TestChokeDoesNotCostTheConnectionOrThePiece verifies that a peer choking us in the middle of a piece, which real
// clients do as a matter of course when they rotate who they upload to, neither drops the connection nor throws away
// what has already arrived, and that the requests it discarded are made again once it unchokes us.
func TestChokeDoesNotCostTheConnectionOrThePiece(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p, done := startTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// The peer unchokes us and we ask it for a piece
	c.NoError(conn.SetDeadline(time.Now().Add(peerMgmtWait)))
	_, err = conn.Write(newTestMessage(unchokeID))
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// A piece three chunks long, of which the first and last have already arrived
	one := &piece{buffer: make([]byte, 3*chunkSize), timeout: time.Now().Add(-time.Minute)}
	one.spans.Insert(&spanlist.Span{Start: 0, Length: chunkSize})
	one.spans.Insert(&spanlist.Span{Start: 2 * chunkSize, Length: chunkSize})
	p.lock.Lock()
	p.pieces[2] = one
	p.lock.Unlock()

	p.resumeDownloads()
	c.Equal(newTestPieceRequest(requestID, 2, chunkSize, chunkSize), <-p.writeQueue)
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

// TestReadDeadlines verifies that we're willing to wait longer than the keep-alive period for the next message from a
// peer, since a peer with nothing to say would otherwise be disconnected and blocked, while the remainder of a message
// that has started to arrive is still expected promptly.
func TestReadDeadlines(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	_, err = conn.Write(newTestMessage(haveID, 0, 0, 0, 0)[:4])
	c.NoError(err)
	rest := nextReadDeadline(t, recorder)
	c.True(rest <= msgReadDeadline, "waiting for the rest of a message must be no more than %v, was %v",
		msgReadDeadline, rest)
}

// TestOversizedMessageIsRejected verifies that a peer can't demand an arbitrarily large allocation from us.
func TestOversizedMessageIsRejected(t *testing.T) {
	d, err := dispatcher.NewDispatcher()
	check.New(t).NoError(err)
	defer d.Stop()
	for _, one := range []struct {
		name   string
		length uint32
	}{
		{name: "one byte too large", length: maxMessageLength + 1},
		{name: "as large as the length prefix allows", length: math.MaxUint32},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			conn, _, done := startTestPeer(t, newTestClient(d))
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

// TestLargestAllowedMessageIsAccepted verifies that the message size limit isn't off by one.
func TestLargestAllowedMessageIsAccepted(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	conn, _, done := startTestPeer(t, newTestClient(d))
	defer xio.CloseIgnoringErrors(conn)

	// An unknown message ID is ignored, no matter how large it is, as long as it is within the limit
	_, err = conn.Write(newTestMessage(200, make([]byte, maxMessageLength-1)...))
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
	d, err := dispatcher.NewDispatcher()
	check.New(t).NoError(err)
	defer d.Stop()
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	_, err = io.ReadFull(conn, buffer)
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	_, err = io.ReadFull(conn, buffer)
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

// TestPieceRequestFloodIsRejected verifies that a peer that asks for far more than we can deliver is disconnected
// rather than allowed to grow the pending request queue without bound.
func TestPieceRequestFloodIsRejected(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
		if _, err = conn.Write(newTestPieceRequest(requestID, uint32(i%testPieceCount), 0, chunkSize)); err != nil {
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	d, err := dispatcher.NewDispatcher()
	check.New(t).NoError(err)
	defer d.Stop()
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)
	otherConn, other := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(otherConn)

	// The first peer still has an expired download recorded for a piece the second peer has since claimed
	p.lock.Lock()
	p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Minute)}
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
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	p.pieces[0] = &piece{buffer: make([]byte, chunkSize), timeout: time.Now().Add(-time.Second)}
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
			d, err := dispatcher.NewDispatcher()
			c.NoError(err)
			defer d.Stop()
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

func newTestClient(d *dispatcher.Dispatcher) *Client {
	c := &Client{
		InRate:              d.InRate.New(math.MaxInt32),
		OutRate:             d.OutRate.New(math.MaxInt32),
		dispatcher:          d,
		torrentFile:         newTestTorrentFile(),
		logger:              slog.New(slog.DiscardHandler),
		peerWaitGroup:       &sync.WaitGroup{},
		peerMgmtStop:        make(chan struct{}),
		peers:               make(map[net.Conn]*peer),
		stoppedChan:         make(chan bool, 1),
		concurrentDownloads: 4,
		peersWanted:         32,
	}
	c.tracker = newTracker(c)
	return c
}

func newTestTorrentFile() *tfs.File {
	var f tfs.File
	f.Info.Name = "test"
	f.Info.PieceLength = chunkSize
	f.Info.Pieces = make([]byte, sha1Size*testPieceCount)
	f.Info.Length = chunkSize * testPieceCount
	return &f
}
