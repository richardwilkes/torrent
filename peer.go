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
	"io"
	"log/slog"
	"maps"
	"net"
	"sync"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/rate"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/container/fixedbits"
	"github.com/richardwilkes/torrent/container/spanlist"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tio"
)

const (
	// idleReadDeadline is how long we'll wait for the next message from a peer. It must be longer than the period
	// peers send keep-alive messages at, otherwise a peer with nothing to say will be disconnected.
	idleReadDeadline        = keepAlivePeriod + 30*time.Second
	msgReadDeadline         = 5 * time.Second
	msgWriteDeadline        = 5 * time.Second
	keepAlivePeriod         = 2 * time.Minute
	downloadReadDeadline    = 10 * time.Second
	maxWaitForChunkDownload = 20 * time.Second
	// maxChokedDownloadWait is how long we'll hold onto a piece we're in the middle of downloading while the peer is
	// choking us. A choked peer has been told to discard our requests, so the time it spends choking us can't be held
	// against it, but the piece can't stay claimed forever either, since no other peer can take it while it is.
	maxChokedDownloadWait = time.Minute
	chunkSize             = dispatcher.ChunkSize
	// maxPendingPieceRequests is the number of unfulfilled piece requests we'll hold onto for a peer before deciding
	// it is flooding us.
	maxPendingPieceRequests = 512
)

// errStorageClosed is returned when a peer tries to read or write the torrent's storage after the client has closed
// it, which can happen for the peer goroutines that outlive the client's peer wait group.
var errStorageClosed = errors.New("torrent storage has been closed")

// useRate accounts for the given number of bytes moved on this connection, charging the limiter in pieces no larger
// than dispatcher.MaxPieceMessageLength, which every permitted cap is large enough to accept. A limiter refuses any
// single amount larger than its cap outright, so charging a whole message at once would tear down the connection to
// a peer over a message that is perfectly legal but bigger than the cap — a bit field for a torrent with more than
// ~131,000 pieces is one, and those are exchanged by both ends as soon as a connection is established.
func useRate(limiter rate.Limiter, amount int) error {
	for amount > 0 {
		charge := min(amount, dispatcher.MaxPieceMessageLength)
		if err := <-limiter.Use(charge); err != nil {
			return err
		}
		amount -= charge
	}
	return nil
}

const (
	chokeID byte = iota
	unchokeID
	interestedID
	notInterestedID
	haveID
	bitFieldID
	requestID
	pieceID
	cancelID
	portID
)

type peer struct {
	client      *Client
	logger      *slog.Logger
	conn        net.Conn
	created     time.Time
	has         *fixedbits.Bits
	requestChan chan *pieceRequest
	writeQueue  chan []byte
	// stateChanged tells the goroutine that delivers our state to the peer that there is something pending. It has a
	// capacity of one and is only ever sent to without blocking, since all that goroutine needs to know is that
	// something is waiting for it and it re-reads all of the pending state each time it wakes.
	stateChanged chan struct{}
	pieces       map[int]*piece // protected by lock
	// haveQueue holds the pieces we've acquired that the peer has yet to be told about. Protected by lock.
	haveQueue []int
	peerState      // protected by lock
	bail      bool // protected by lock
	// toldChoking and toldInterested are what the peer was last told about our choking and our interest. They trail
	// amChoking and amInterested until the message saying so has been queued up. Both are protected by lock.
	toldChoking    bool
	toldInterested bool
	lock           sync.RWMutex
}

type pieceRequest struct {
	index  int
	begin  int
	length int
	cancel bool
}

// maxMessageLength returns the largest message length, which includes the one-byte message ID but not the four-byte
// length prefix, that will be accepted from this peer. Every message but the bit field has a small, fixed size, and
// the largest of those is a piece message carrying a full chunk. A bit field is exactly one byte per eight pieces in
// the torrent, so the limit is what this particular torrent needs rather than what the largest permitted one would:
// a peer on a small torrent then can't make us allocate for a message that torrent could never produce.
func (p *peer) maxMessageLength() uint32 {
	// The bit length is fixed when the peer is created, so no lock is needed to look at it.
	return uint32(max(dispatcher.MaxPieceMessageLength-4, 1+p.has.ByteLength()))
}

// validMessageLength returns true if the given message length, which includes the one-byte message ID, is valid for a
// message with the given ID. Unknown IDs are accepted, since they are ignored when processed. The bit field message is
// only checked for a minimum length here, since its exact length must be verified against the size of the bit field
// when it is processed.
func validMessageLength(id byte, length uint32) bool {
	switch id {
	case chokeID, unchokeID, interestedID, notInterestedID:
		return length == 1
	case haveID:
		return length == 5
	case bitFieldID:
		return length >= 1
	case requestID, cancelID:
		return length == 13
	case pieceID:
		// A piece message with no chunk data at all carries no progress, so it is refused rather than allowed to pass
		// through the code that decides whether a peer is still delivering what we asked it for
		return length > 9
	case portID:
		return length == 3
	default:
		return true
	}
}

func newPieceRequest(buffer []byte, cancel bool) *pieceRequest {
	return &pieceRequest{
		index:  int(binary.BigEndian.Uint32(buffer[1:5])),
		begin:  int(binary.BigEndian.Uint32(buffer[5:9])),
		length: int(binary.BigEndian.Uint32(buffer[9:])),
		cancel: cancel,
	}
}

// validPieceRequest returns true if the request is for a range of data that actually exists within the torrent and is
// no larger than a single chunk. Requests from a peer are unverified data, so a request that isn't valid is a sign of
// either a broken or a hostile peer.
func (p *peer) validPieceRequest(req *pieceRequest) bool {
	if req.length < 1 || req.length > chunkSize || req.begin < 0 {
		return false
	}
	if req.index < 0 || req.index >= p.client.torrentFile.PieceCount() {
		return false
	}
	return int64(req.begin)+int64(req.length) <= p.client.torrentFile.LengthOf(req.index)
}

type piece struct {
	spans   spanlist.SpanList
	timeout time.Time
	buffer  []byte
	lock    sync.RWMutex
}

func newPeer(client *Client, conn net.Conn, logger *slog.Logger) *peer {
	return &peer{
		client:       client,
		logger:       logger,
		conn:         conn,
		created:      time.Now(),
		has:          fixedbits.New(client.torrentFile.PieceCount()),
		requestChan:  make(chan *pieceRequest),
		writeQueue:   make(chan []byte, 32),
		stateChanged: make(chan struct{}, 1),
		pieces:       make(map[int]*piece),
		// Both ends of a new connection start out choked and uninterested, so there is nothing to tell the peer about
		// until one of those changes.
		toldChoking: true,
		peerState: peerState{
			amChoking:   true,
			peerChoking: true,
		},
	}
}

type peerState struct {
	lastReceived    time.Time
	downloadStarted time.Time
	bytesRead       int64
	bytesWritten    int64
	amChoking       bool
	amInterested    bool
	peerChoking     bool
	peerInterested  bool
	downloading     bool
}

// lastProgress returns the time the most recent progress was made on a download, which is either when a chunk was
// last received or when the current download was started, whichever is later. Note that the download start time is
// needed because a peer that has just been asked for a piece has yet to send us anything for it.
func (s *peerState) lastProgress() time.Time {
	if s.lastReceived.Before(s.downloadStarted) {
		return s.downloadStarted
	}
	return s.lastReceived
}

// downloadStalled returns true if no progress has been made on the current download within the allowed time.
func (s *peerState) downloadStalled(now time.Time) bool {
	return now.Sub(s.lastProgress()) > maxWaitForChunkDownload
}

// updateInterest recomputes whether we're interested in what this peer has, records it, and makes sure the peer gets
// told about any change. It is called from both the goroutine reading messages from the peer and the one managing
// peers, so amInterested is only ever recorded while the lock is held and the delivery of the message is left to
// processStateChanges.
func (p *peer) updateInterest() peerState {
	p.clearExpiredDownloads()
	p.lock.RLock()
	has := p.has.Clone()
	downloading := len(p.pieces) > 0
	p.lock.RUnlock()
	interested := downloading || p.client.tracker.isInteresting(has)
	p.lock.Lock()
	p.amInterested = interested
	changed := p.toldInterested != interested
	ps := p.peerState
	ps.downloading = downloading
	p.lock.Unlock()
	if changed {
		p.signalStateChange()
	}
	return ps
}

// setChoked records whether we're choking the peer, leaving the delivery of the message saying so to
// processStateChanges.
func (p *peer) setChoked(choked bool) {
	p.lock.Lock()
	changed := p.amChoking != choked
	p.amChoking = choked
	p.lock.Unlock()
	if changed {
		p.signalStateChange()
	}
}

// queueHave records that the peer has yet to be told that we now have the given piece, leaving the delivery of the
// message saying so to processStateChanges.
func (p *peer) queueHave(index int) {
	p.lock.Lock()
	p.haveQueue = append(p.haveQueue, index)
	p.lock.Unlock()
	p.signalStateChange()
}

// signalStateChange lets the goroutine delivering our state to the peer know that something is pending. The signal is
// never allowed to block: it says nothing more than that there is something to look at, and the delivery goroutine
// re-reads all of the pending state when it wakes, so a signal that finds one already waiting has nothing to add.
// That matters because sending to the write queue blocks once that queue fills up: peer management walks every peer in
// turn and a piece that finishes validating is announced to every peer from the read goroutine of the one that
// completed it, so a caller left waiting on one peer that can't be written to would stall all of the others.
func (p *peer) signalStateChange() {
	select {
	case p.stateChanged <- struct{}{}:
	default:
	}
}

// processStateChanges delivers the messages that tell the peer about our state: whether we're choking it, whether
// we're interested in what it has, and which pieces we've acquired. A single goroutine owns their delivery, so they
// can't be sent out of order or in duplicate, which would leave the remote's notion of our state permanently at odds
// with ours.
func (p *peer) processStateChanges(done chan bool) {
	for {
		select {
		case <-p.stateChanged:
			for buffer := p.nextStateMessage(); buffer != nil; buffer = p.nextStateMessage() {
				select {
				case p.writeQueue <- buffer:
				case <-done:
					return
				}
			}
		case <-done:
			return
		}
	}
}

// nextStateMessage returns the next message needed to bring what the peer has been told about our state in line with
// what it actually is, or nil if the peer is already current.
func (p *peer) nextStateMessage() []byte {
	p.lock.Lock()
	defer p.lock.Unlock()
	switch {
	case p.toldChoking != p.amChoking:
		p.toldChoking = p.amChoking
		if p.amChoking {
			return newStateMessage(chokeID)
		}
		return newStateMessage(unchokeID)
	case p.toldInterested != p.amInterested:
		p.toldInterested = p.amInterested
		if p.amInterested {
			return newStateMessage(interestedID)
		}
		return newStateMessage(notInterestedID)
	case len(p.haveQueue) != 0:
		index := p.haveQueue[0]
		if len(p.haveQueue) == 1 {
			p.haveQueue = nil
		} else {
			p.haveQueue = p.haveQueue[1:]
		}
		return newHaveMessage(index)
	default:
		return nil
	}
}

// newStateMessage creates one of the messages that consists of nothing more than its ID.
func newStateMessage(id byte) []byte {
	buffer := make([]byte, 5)
	binary.BigEndian.PutUint32(buffer[:4], 1)
	buffer[4] = id
	return buffer
}

// newHaveMessage creates the message that tells a peer we have a piece.
func newHaveMessage(index int) []byte {
	buffer := make([]byte, 9)
	binary.BigEndian.PutUint32(buffer[:4], 5)
	buffer[4] = haveID
	binary.BigEndian.PutUint32(buffer[5:], uint32(index))
	return buffer
}

// newBitFieldMessage creates the message that tells a peer which pieces we have.
func newBitFieldMessage(bits []byte) []byte {
	buffer := make([]byte, 5+len(bits))
	binary.BigEndian.PutUint32(buffer[:4], uint32(1+len(bits)))
	buffer[4] = bitFieldID
	copy(buffer[5:], bits)
	return buffer
}

// newRequestMessage creates the message that asks a peer for a chunk of a piece.
func newRequestMessage(index, begin, length int) []byte {
	buffer := make([]byte, 17)
	binary.BigEndian.PutUint32(buffer[:4], 13)
	buffer[4] = requestID
	binary.BigEndian.PutUint32(buffer[5:9], uint32(index))
	binary.BigEndian.PutUint32(buffer[9:13], uint32(begin))
	binary.BigEndian.PutUint32(buffer[13:], uint32(length))
	return buffer
}

// bailOut records that we're done with this peer and closes the connection. Closing it is what actually ends the
// exchange: the read loop only looks at the flag between messages and may be parked waiting up to idleReadDeadline for
// the next one, which would leave a peer we've already given up on holding one of the limited peer slots, and being
// walked by peer management, for minutes.
func (p *peer) bailOut() {
	p.lock.Lock()
	p.bail = true
	p.lock.Unlock()
	xio.CloseIgnoringErrors(p.conn)
}

// blockAddressOnReadFailure blocks the peer's address unless the failure was routine churn rather than something the
// peer did wrong. A remote that simply hangs up, and a connection that we closed ourselves — peer rotation, making
// room for an incoming connection, or bailing out on a piece — would otherwise leave a well-behaved peer banned from
// both dialing us and being dialed for the full block duration, which can leave a small swarm with no usable peers.
func (p *peer) blockAddressOnReadFailure(err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return
	}
	p.lock.RLock()
	bailing := p.bail
	p.lock.RUnlock()
	if bailing {
		return
	}
	p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
}

func (p *peer) processIncomingMessages() {
	defer func() {
		close(p.requestChan)
		p.writeQueue <- nil
		p.lock.Lock()
		p.bail = true
		for {
			list := make([]int, 0, len(p.pieces))
			for index := range p.pieces {
				list = append(list, index)
			}
			p.pieces = make(map[int]*piece)
			p.lock.Unlock()
			for _, index := range list {
				p.client.tracker.clearDownload(index, p)
			}
			p.lock.Lock()
			if len(p.pieces) == 0 {
				p.lock.Unlock()
				// Notify other peers on the client to check for potential downloads, since we may have freed up some
				// pieces to download
				for _, other := range p.client.currentPeers() {
					if other != p {
						other.startDownloadIfNeeded()
					}
				}
				return
			}
		}
	}()
	// The bit field has to be the first message we send, so it is queued up before anything else can be. Without it, a
	// peer that connects once we already have pieces — which is every peer that connects to a client that is seeding,
	// since the pieces are validated before any peer exists — would see us as having nothing and never ask us for
	// anything. It is only sent when we actually have something to advertise, since a peer that has nothing is free to
	// leave it out entirely.
	if bits := p.client.tracker.bitField(); bits != nil {
		p.writeQueue <- newBitFieldMessage(bits)
	}
	// done is closed once nothing more can be written to the peer, releasing the goroutines that would otherwise be
	// left waiting on a write queue that no longer has a reader.
	done := make(chan bool)
	go p.processWriteQueue(done)
	go p.processStateChanges(done)
	go p.pieceRequestQueue()
	lengthBuffer := make([]byte, 4)
	maxLength := p.maxMessageLength()
	for {
		p.lock.RLock()
		bail := p.bail
		p.lock.RUnlock()
		if bail {
			p.logger.Warn("Piece download taking too long, closing connection to peer")
			return
		}
		if err := tio.ReadWithDeadline(p.conn, lengthBuffer, idleReadDeadline); err != nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(p.logger, err)
			}
			p.blockAddressOnReadFailure(err)
			return
		}
		length := binary.BigEndian.Uint32(lengthBuffer)
		if length > maxLength {
			p.logger.Warn("message too large", "length", length, "max", maxLength)
			p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
			return
		}
		if length > 0 { // Not a keep-alive message
			buffer := make([]byte, length)
			if err := tio.ReadWithDeadline(p.conn, buffer, msgReadDeadline); err != nil {
				if tio.ShouldLogIOError(err) {
					errs.LogTo(p.logger, err)
				}
				p.blockAddressOnReadFailure(err)
				return
			}
			if !validMessageLength(buffer[0], length) {
				p.logger.Warn("invalid message length", "id", int(buffer[0]), "length", length)
				p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
				return
			}
			switch buffer[0] {
			case chokeID:
				p.lock.Lock()
				p.peerChoking = true
				p.lock.Unlock()
				p.suspendDownloads()
			case unchokeID:
				p.lock.Lock()
				p.peerChoking = false
				p.lock.Unlock()
				p.resumeDownloads()
				p.startDownloadIfNeeded()
			case interestedID:
				p.lock.Lock()
				p.peerInterested = true
				p.lock.Unlock()
			case notInterestedID:
				p.lock.Lock()
				p.peerInterested = false
				p.lock.Unlock()
			case haveID:
				index := int(binary.BigEndian.Uint32(buffer[1:]))
				p.lock.Lock()
				p.has.Set(index)
				p.lock.Unlock()
				p.updateInterest()
				p.startDownloadIfNeeded()
			case bitFieldID:
				p.lock.Lock()
				if length != uint32(1+p.has.ByteLength()) {
					p.lock.Unlock()
					p.logger.Warn("unexpected bit field length", "expected", p.has.ByteLength(), "actual", length-1)
					p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
					return
				}
				p.has.SetBytes(buffer[1:])
				p.lock.Unlock()
				p.updateInterest()
			case requestID:
				req := newPieceRequest(buffer, false)
				if !p.validPieceRequest(req) {
					p.logger.Warn("invalid piece request", "index", req.index, "begin", req.begin, "length", req.length)
					p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
					return
				}
				p.lock.RLock()
				canRequest := !p.amChoking
				p.lock.RUnlock()
				if canRequest {
					p.requestChan <- req
				}
			case pieceID:
				if err := p.receivedChunk(int(binary.BigEndian.Uint32(buffer[1:5])), int(binary.BigEndian.Uint32(buffer[5:9])), buffer[9:]); err != nil {
					if tio.ShouldLogIOError(err) {
						errs.LogTo(p.logger, err)
					}
					return
				}
			case cancelID:
				p.requestChan <- newPieceRequest(buffer, true)
			case portID:
				// Ignore. DHT not implemented.
			default:
				p.logger.Warn("unknown ID", "id", int(buffer[0]))
			}
		}
		p.lock.Lock()
		p.bytesRead += int64(length + 4)
		p.lock.Unlock()
		p.client.tracker.addDownloadedBytes(int64(length + 4))
		if err := useRate(p.client.InRate, int(length+4)); err != nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(p.logger, err)
			}
			return
		}
	}
}

// suspendDownloads stops the deadline on the pieces we're in the middle of downloading from running out while the peer
// is choking us. Choking is normal protocol behavior — most clients rotate who they upload to — and BEP 3 has the peer
// discard whatever we've asked for while it is in effect, so time spent choked can't be held against the peer or we'd
// drop the connection, and the partial piece with it, every time one chokes us mid-piece. The deadline is pushed out
// rather than removed, since the piece can't stay claimed forever if the peer never unchokes us again.
func (p *peer) suspendDownloads() {
	p.lock.RLock()
	pieces := maps.Clone(p.pieces)
	p.lock.RUnlock()
	deadline := time.Now().Add(maxChokedDownloadWait)
	for _, one := range pieces {
		one.lock.Lock()
		one.timeout = deadline
		one.lock.Unlock()
	}
}

// resumeDownloads restarts the deadline on the pieces we're in the middle of downloading and asks again for the chunks
// that haven't arrived, since a peer that was choking us discarded the requests we'd already made.
func (p *peer) resumeDownloads() {
	p.lock.Lock()
	pieces := maps.Clone(p.pieces)
	now := time.Now()
	if len(pieces) != 0 {
		p.downloadStarted = now
	}
	p.lock.Unlock()
	deadline := now.Add(downloadReadDeadline)
	for index, one := range pieces {
		var requests [][]byte
		one.lock.Lock()
		one.timeout = deadline
		for i := 0; i < len(one.buffer); i += chunkSize {
			size := min(len(one.buffer)-i, chunkSize)
			if !one.spans.Contains(&spanlist.Span{Start: i, Length: size}) {
				requests = append(requests, newRequestMessage(index, i, size))
			}
		}
		one.lock.Unlock()
		for _, buffer := range requests {
			p.writeQueue <- buffer
		}
	}
}

func (p *peer) startDownloadIfNeeded() {
	var has *fixedbits.Bits
	p.lock.RLock()
	if !p.bail && !p.peerChoking && len(p.pieces) == 0 {
		has = p.has.Clone()
	}
	p.lock.RUnlock()
	if has != nil {
		if index := p.client.tracker.selectForDownloading(p, has); index != -1 {
			p.queuePieceDownload(index)
		}
	}
}

func (p *peer) queuePieceDownload(index int) {
	length := int(p.client.torrentFile.LengthOf(index))
	p.lock.Lock()
	// The piece has to be given back if we're already on our way out. Our teardown releases everything it finds in the
	// map and then leaves for good, so a piece recorded after that would be left marked as being downloaded by a peer
	// that no longer exists, with nothing to ever release it and no other peer able to take it.
	bailing := p.bail
	_, ok := p.pieces[index]
	if !bailing && !ok {
		now := time.Now()
		p.pieces[index] = &piece{
			buffer:  make([]byte, length),
			timeout: now.Add(downloadReadDeadline),
		}
		p.downloadStarted = now
	}
	p.lock.Unlock()
	if bailing {
		p.client.tracker.clearDownload(index, p)
		return
	}
	if !ok {
		for i := 0; i < length; i += chunkSize {
			p.writeQueue <- newRequestMessage(index, i, min(length-i, chunkSize))
		}
	}
}

func (p *peer) receivedChunk(index, begin int, buffer []byte) error {
	p.lock.RLock()
	one, ok := p.pieces[index]
	p.lock.RUnlock()
	if !ok {
		return errs.Newf("received unrequested piece %d", index)
	}
	// The offset is unverified data from the peer. On a platform where int is 32 bits a large enough one arrives as a
	// negative number, which would make the range that is checked below smaller than the one that is actually written,
	// so the sign is checked in its own right and the end of the range is computed without being able to overflow.
	if begin < 0 || len(buffer) > len(one.buffer) || begin > len(one.buffer)-len(buffer) {
		p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
		return errs.Newf("piece %d would overrun buffer", index)
	}
	last := begin + len(buffer)
	span := spanlist.Span{Start: begin, Length: len(buffer)}
	one.lock.Lock()
	now := time.Now()
	bailIfNotFinish := one.timeout.Before(now)
	// A chunk that carries nothing we don't already have makes no progress, so it must not renew either of the
	// deadlines that decide whether this peer is still delivering the piece it was asked for. A peer that dribbles out
	// duplicate chunks would otherwise hold onto a piece indefinitely, at almost no cost to itself, and no other peer
	// could take it.
	progressed := !one.spans.Contains(&span)
	if progressed {
		one.timeout = now.Add(downloadReadDeadline)
	}
	copy(one.buffer[begin:last], buffer)
	one.spans.Insert(&span)
	if progressed {
		p.lock.Lock()
		p.lastReceived = now
		p.lock.Unlock()
	}
	if len(one.spans.Spans) == 1 && one.spans.Spans[0].Start == 0 && one.spans.Spans[0].Length == len(one.buffer) {
		if p.client.torrentFile.Validate(index, one.buffer) {
			var n int
			err := errStorageClosed
			if f := p.client.storageFile(); f != nil {
				n, err = f.WriteAt(one.buffer, p.client.torrentFile.OffsetOf(index))
			}
			one.lock.Unlock()
			p.lock.Lock()
			delete(p.pieces, index)
			p.lock.Unlock()
			if err != nil && (!errors.Is(err, io.EOF) || n != len(one.buffer)) {
				p.client.tracker.clearDownload(index, p)
				errs.LogTo(p.logger, errs.NewWithCause("unable to write piece", err), "index", index)
			} else {
				p.client.tracker.markBlockValid(index)
				p.client.tracker.setProgress(-1)
				if p.client.tracker.isDownloadComplete() {
					p.client.tracker.setState(Seeding)
				}
			}
			p.updateInterest()
			p.startDownloadIfNeeded()
		} else {
			one.lock.Unlock()
			p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
			return errs.Newf("discarding invalid piece %d", index)
		}
	} else {
		one.lock.Unlock()
		if bailIfNotFinish {
			p.bailOut()
		}
	}
	return nil
}

// pendingRequests holds the piece requests that have been received from a peer, but have not been fulfilled yet, in
// the order they were received.
type pendingRequests struct {
	queue map[int]*pieceRequest
	head  int
	tail  int
}

func newPendingRequests() *pendingRequests {
	return &pendingRequests{queue: make(map[int]*pieceRequest)}
}

// count returns the number of requests waiting to be fulfilled.
func (q *pendingRequests) count() int {
	return len(q.queue)
}

// add a request to the end of the queue, or remove the request it cancels. Returns false if the queue is already
// holding as many requests as it is willing to.
func (q *pendingRequests) add(req *pieceRequest) bool {
	if req.cancel {
		for k, one := range q.queue {
			if req.index == one.index && req.begin == one.begin && req.length == one.length {
				delete(q.queue, k)
				break
			}
		}
		return true
	}
	if len(q.queue) >= maxPendingPieceRequests {
		return false
	}
	q.queue[q.head] = req
	q.head++
	return true
}

// next returns the request that should be fulfilled next, or nil if there are none. The request remains in the queue
// until removeNext is called.
func (q *pendingRequests) next() *pieceRequest {
	for q.tail < q.head {
		if req, ok := q.queue[q.tail]; ok {
			return req
		}
		q.tail++
	}
	return nil
}

// removeNext removes the request that the last call to next returned.
func (q *pendingRequests) removeNext() {
	delete(q.queue, q.tail)
	q.tail++
}

func (p *peer) pieceRequestQueue() {
	queueChan := make(chan *pieceRequest)
	go p.processPieceRequests(queueChan)
	defer close(queueChan)
	queue := newPendingRequests()
	flooded := false
	for {
		var req *pieceRequest
		var ok bool
		if next := queue.next(); next == nil {
			if req, ok = <-p.requestChan; !ok {
				return
			}
		} else {
			select {
			case req, ok = <-p.requestChan:
				if !ok {
					for one := queue.next(); one != nil; one = queue.next() {
						queueChan <- one
						queue.removeNext()
					}
					return
				}
			case queueChan <- next:
				queue.removeNext()
				continue
			}
		}
		if !queue.add(req) && !flooded {
			// Drop the connection, but keep draining the channel, since the goroutine feeding it would otherwise be
			// left blocked trying to hand us another request.
			flooded = true
			p.logger.Warn("too many outstanding piece requests", "max", maxPendingPieceRequests)
			p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
			xio.CloseIgnoringErrors(p.conn)
		}
	}
}

func (p *peer) processPieceRequests(in chan *pieceRequest) {
	process := true
	for req := range in {
		if !process {
			continue
		}
		if !p.client.tracker.hasPiece(req.index) {
			// We only ever tell peers about the pieces we have, so this is a broken or hostile peer. Serving the
			// request anyway would hand it whatever the storage happens to hold for a piece we haven't downloaded
			// yet, which the remote would rightly treat as corrupt data and ban us for. There is no way to refuse a
			// request in the base protocol, so it is simply ignored.
			p.logger.Warn("ignoring request for a piece we don't have", "index", req.index, "begin", req.begin,
				"length", req.length)
			continue
		}
		buffer := make([]byte, 13+req.length)
		binary.BigEndian.PutUint32(buffer[:4], uint32(9+req.length))
		buffer[4] = pieceID
		binary.BigEndian.PutUint32(buffer[5:9], uint32(req.index))
		binary.BigEndian.PutUint32(buffer[9:13], uint32(req.begin))
		// This goroutine isn't tracked by the client's peer wait group, so it can still be draining requests after the
		// client has closed the storage file.
		err := errStorageClosed
		if f := p.client.storageFile(); f != nil {
			_, err = f.ReadAt(buffer[13:], p.client.torrentFile.OffsetOf(req.index)+int64(req.begin))
		}
		if err != nil {
			errs.LogTo(p.logger, errs.NewWithCause("unable to read piece", err), "index", req.index, "begin", req.begin, "length", req.length)
			xio.CloseIgnoringErrors(p.conn)
			process = false
			continue
		}
		p.writeQueue <- buffer
	}
}

func (p *peer) processWriteQueue(done chan bool) {
	var lastWriteTime time.Time
	go p.keepAlive(done)
	for buffer := range p.writeQueue {
		var err error
		if buffer != nil {
			if len(buffer) == 4 {
				if time.Since(lastWriteTime) < keepAlivePeriod {
					continue
				}
			} else {
				err = useRate(p.client.OutRate, len(buffer))
			}
			if err == nil {
				lastWriteTime = time.Now()
				err = tio.WriteWithDeadline(p.conn, buffer, msgWriteDeadline)
			}
		}
		if err != nil || buffer == nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(p.logger, err)
			}
			close(done)
			xio.CloseIgnoringErrors(p.conn)
			// Drain any remaining entries in the queue, terminating after a
			// significant delay to allow all writers time to stop posting to
			// the queue.
			for {
				select {
				case <-p.writeQueue:
				case <-time.After(5 * time.Minute):
					return
				}
			}
		}
		p.lock.Lock()
		p.bytesWritten += int64(len(buffer))
		p.lock.Unlock()
		p.client.tracker.addUploadedBytes(int64(len(buffer)))
	}
	close(done)
}

func (p *peer) keepAlive(done chan bool) {
	for {
		select {
		case <-time.After(keepAlivePeriod):
			p.writeQueue <- make([]byte, 4)
		case <-done:
			return
		}
	}
}

func (p *peer) clearExpiredDownloads() {
	p.lock.RLock()
	m := maps.Clone(p.pieces)
	choking := p.peerChoking
	p.lock.RUnlock()
	now := time.Now()
	giveUpOnPeer := false
	for k, v := range m {
		v.lock.RLock()
		remove := v.timeout.Before(now)
		v.lock.RUnlock()
		if !remove {
			continue
		}
		p.lock.Lock()
		// Only a piece we still hold is released. The snapshot above may name one our own teardown has already given
		// up, or one that finished while we were looking, and another peer may have claimed it since: releasing that
		// would take the piece away from the peer that is actually downloading it.
		_, ours := p.pieces[k]
		if ours {
			delete(p.pieces, k)
		}
		p.lock.Unlock()
		if ours {
			p.client.tracker.clearDownload(k, p)
			// A peer that is choking us was told to discard our requests, so having nothing to show for the piece
			// isn't its fault: give up the piece so another peer can take it, but keep the connection.
			giveUpOnPeer = giveUpOnPeer || !choking
		}
	}
	if giveUpOnPeer {
		p.bailOut()
	}
}
