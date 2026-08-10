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
		{name: "piece", id: pieceID, valid: []uint32{9, 10, 9 + chunkSize}, invalid: []uint32{1, 5, 8}},
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

	// Collect the message IDs the peer would have written, rather than sending them over the connection
	var sent []byte
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for buffer := range p.writeQueue {
			sent = append(sent, buffer[4])
		}
	}()

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
	close(p.writeQueue)
	<-drained

	c.True(len(sent) > 0, "no interest was ever expressed")
	for i, id := range sent {
		// We start out uninterested, so the messages must alternate, beginning with an interested one
		expected := interestedID
		if i%2 == 1 {
			expected = notInterestedID
		}
		if id != expected {
			t.Fatalf("message %d was %d, not %d; messages %d through %d were %v", i, id, expected,
				max(0, i-8), i, sent[max(0, i-8):i+1])
		}
	}
	p.lock.RLock()
	amInterested := p.amInterested
	p.lock.RUnlock()
	c.Equal(amInterested, sent[len(sent)-1] == interestedID, "the last message must match the interest we recorded")
}

// TestUpdateInterestDoesNotBlockOnAFullWriteQueue verifies that a peer whose write queue has backed up doesn't hold up
// the goroutines that only need to reassess our interest in it. Peer management walks every peer in turn, so one peer
// that can't be written to must not be able to stall the management of all the others.
func TestUpdateInterestDoesNotBlockOnAFullWriteQueue(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	conn, p := newTestPeer(t, client)
	defer xio.CloseIgnoringErrors(conn)

	// Fill the write queue, so that the next message sent to it has nowhere to go
	for range cap(p.writeQueue) {
		p.writeQueue <- make([]byte, 4)
	}

	// The peer has a piece we want, so the first update has an interested message to deliver and blocks on the queue
	p.lock.Lock()
	p.has.Set(0)
	p.lock.Unlock()
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		p.updateInterest()
	}()
	waitForInterestDelivery(t, p)
	select {
	case <-blocked:
		t.Fatal("the update should still be blocked on the full write queue")
	default:
	}

	// Peer management must still be able to reassess this peer without waiting for the queue to drain
	waitFor(t, "updateInterest", func() { p.updateInterest() })

	// Draining the queue lets the blocked delivery finish
	<-p.writeQueue
	select {
	case <-blocked:
	case <-time.After(peerMgmtWait):
		t.Fatal("the delivery never completed once the queue drained")
	}
	p.lock.RLock()
	interested := p.amInterested
	told := p.toldInterested
	p.lock.RUnlock()
	c.True(interested)
	c.Equal(interested, told, "the peer must end up knowing what we recorded")
}

// waitForInterestDelivery fails the test if no goroutine takes on the delivery of the interest messages, which is the
// point past which it can no longer make progress against a full write queue.
func waitForInterestDelivery(t *testing.T, p *peer) {
	t.Helper()
	deadline := time.Now().Add(peerMgmtWait)
	for {
		p.lock.RLock()
		sending := p.sendingInterest
		p.lock.RUnlock()
		if sending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the interest message was never taken up for delivery")
		}
		time.Sleep(time.Millisecond)
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
