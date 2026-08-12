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
	"slices"
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
	idleReadDeadline = keepAlivePeriod + 30*time.Second
	msgReadDeadline  = 5 * time.Second
	// minMessageRate is the slowest, in bytes per second, that a message is expected to move at. A message is given the
	// longer of the flat deadline for its direction and what carrying it at this rate would take, rather than the flat
	// deadline alone, because the bit field both ends exchange as soon as they connect is one byte for every eight
	// pieces of the torrent — a quarter of a megabyte at tfs.MaxPieceCount — and a flat few seconds for that amounts to
	// demanding a transfer rate that a slow but perfectly honest peer has no way to meet. Timing one out costs more
	// than the connection, too: on the reading side a deadline is neither EOF nor a connection we closed, so
	// blockAddressOnReadFailure bans the address as well and the peer can't come back.
	minMessageRate          = 8 * 1024
	msgWriteDeadline        = 5 * time.Second
	keepAlivePeriod         = 2 * time.Minute
	downloadReadDeadline    = 10 * time.Second
	maxWaitForChunkDownload = 20 * time.Second
	// maxChokedDownloadWait is how long we'll hold onto a piece we're in the middle of downloading while the peer is
	// choking us. A choked peer has been told to discard our requests, so the time it spends choking us can't be held
	// against it, but the piece can't stay claimed forever either, since no other peer can take it while it is.
	maxChokedDownloadWait = time.Minute
	// maxDownloadStall is the longest a piece may stay claimed by a peer without any of it arriving, no matter what the
	// peer does. The deadline for a piece is pushed out each time the peer chokes us and restarted each time it
	// unchokes us, both of which are legitimate, so a remote that alternates the two never has to deliver anything at
	// all: this is the bound that nothing but data we don't already have can renew.
	maxDownloadStall = 2 * maxChokedDownloadWait
	chunkSize        = dispatcher.ChunkSize
	// maxPendingPieceRequests is the number of unfulfilled piece requests we'll hold onto for a peer before deciding
	// it is flooding us.
	maxPendingPieceRequests = 512
	// maxOutstandingChunkRequests is the number of chunk requests we'll keep in front of a single peer at a time. A
	// piece is asked for a chunk at a time and a piece at tfs.MaxPieceLength is 2048 of them, so asking for a whole
	// piece in one burst puts far more requests in front of a peer than clients are willing to hold: this library's
	// own upload side treats more than maxPendingPieceRequests as flooding and bans the address for it, which leaves
	// two instances of it unable to transfer a torrent with a piece length above 8MB at all, and third party clients
	// with request queue caps of their own — commonly 250 to 2000 — drop the excess or hang up, leaving the piece to
	// stall until the peer is given up on and banned. This sits far enough below every one of those caps to leave room
	// for the batch a peer is re-asked for after a choke overlapping one still in flight, while still keeping 2MB in
	// flight per peer, which is well beyond what the delay between asking and being answered can hold.
	maxOutstandingChunkRequests = 128
	// requestMessageLength is the size of a message asking a peer for a chunk, length prefix included.
	requestMessageLength = 17
)

// errStorageClosed is returned when a peer tries to read or write the torrent's storage after the client has closed
// it, which can happen for the peer goroutines that outlive the client's peer wait group.
var errStorageClosed = errors.New("torrent storage has been closed")

// messageReadDeadline returns how long the body of a message of the given length, which includes the one-byte message
// ID, is allowed to take to arrive: the longer of the minimum every message is given and what carrying it at
// minMessageRate would take.
func messageReadDeadline(length uint32) time.Duration {
	return max(msgReadDeadline, time.Duration(length)*time.Second/minMessageRate)
}

// messageWriteDeadline returns how long a message of the given length, which here includes its four-byte length
// prefix, is allowed to take to go out. A peer that reads slowly is no more at fault than one that writes slowly, and
// the message that makes the difference is the same one: the bit field we send a peer the moment it connects is as big
// as the one it sends us.
func messageWriteDeadline(length int) time.Duration {
	return max(msgWriteDeadline, time.Duration(length)*time.Second/minMessageRate)
}

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
	client *Client
	logger *slog.Logger
	conn   net.Conn
	// lastWriteTime is when we last put something on the wire for this peer, which is what the keep-alive is measured
	// from. Protected by lock.
	lastWriteTime time.Time
	created       time.Time
	has           *fixedbits.Bits
	requestChan   chan *pieceRequest
	writeQueue    chan []byte
	// chokedChan tells the goroutine holding this peer's outstanding piece requests that we have begun choking it, so
	// that it can discard them. It has a capacity of one and is only ever sent to without blocking, since it says
	// nothing more than that our choking state is worth looking at again.
	chokedChan chan struct{}
	// stateChanged tells the goroutine that delivers our state to the peer that there is something pending. It has a
	// capacity of one and is only ever sent to without blocking, since all that goroutine needs to know is that
	// something is waiting for it and it re-reads all of the pending state each time it wakes.
	stateChanged chan struct{}
	// chunkRequestsChanged tells the goroutine that asks the peer for the chunks we need that there is something
	// pending. It has a capacity of one and is only ever sent to without blocking, for the same reasons stateChanged
	// is.
	chunkRequestsChanged chan struct{}
	// pendingChunkRequests holds the pieces we're downloading from this peer whose outstanding chunks have yet to be
	// asked for. Protected by lock.
	pendingChunkRequests map[int]bool
	pieces               map[int]*piece // protected by lock
	// haveQueue holds the pieces we've acquired that the peer has yet to be told about. Protected by lock.
	haveQueue []int
	// cancelQueue holds the messages taking back chunk requests the peer has yet to be told we no longer want, built
	// whole rather than as indexes because what is being withdrawn is a set of blocks within a piece we've already
	// dropped every other record of. Protected by lock.
	cancelQueue [][]byte
	peerState        // protected by lock
	bail        bool // protected by lock
	// toldChoking and toldInterested are what the peer was last told about our choking and our interest. They trail
	// amChoking and amInterested until the message saying so has been queued up. Both are protected by lock.
	toldChoking    bool
	toldInterested bool
	lock           sync.RWMutex
	// interestLock serializes updateInterest from end to end. It is deliberately not the lock above, since the
	// decision has to be made with the tracker's lock in hand and taking that one while holding the peer's would
	// order the two.
	interestLock sync.Mutex
	// downloadStartLock serializes startDownloadIfNeeded from end to end, for the reason interestLock exists and under
	// the same restriction: whether the peer may take a piece on is decided from a snapshot of what it already holds
	// and acted on afterwards, with the tracker's lock taken in between, so two callers left to interleave both find
	// it holding nothing and both claim. The peer's own read goroutine reaches it from every unchoke, have, bit field
	// and completed piece, and the peer management goroutine reaches it through clearExpiredDownloads, so the two run
	// into each other as a matter of course.
	downloadStartLock sync.Mutex
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
	// lastProgress is when a chunk of this piece that we didn't already have last arrived, or when the piece was
	// claimed if none has. Unlike timeout, which the peer choking and unchoking us pushes out, nothing but actual
	// progress renews it, so it is what bounds how long a peer that never delivers can keep the piece to itself.
	lastProgress time.Time
	buffer       []byte
	// askedTo is how far into the piece the requests built so far reach. Only what lies beyond it is asked for when
	// more of the piece can be put in front of the peer, so a chunk isn't asked for a second time while we're still
	// waiting for it. A peer that chokes us discards everything it was asked for, so resumeDownloads winds this back
	// to the beginning and the whole of what is still missing is asked for again.
	askedTo int
	// inFlight is the number of this piece's chunks that have been asked for and haven't arrived. Summed across the
	// pieces a peer holds, it is what says how much of maxOutstandingChunkRequests is still free.
	inFlight int
	// requested is set once the messages asking for the chunks we still need of this piece have begun going out on the
	// wire, which is where the deadline for the piece starts. Until then the peer hasn't been asked for anything and
	// can't be held to a deadline: the requests wait their turn on the shared write queue, which for a peer we're also
	// uploading to under a cap is backed up for tens of seconds. Only maxDownloadStall, which nothing renews, bounds a
	// claim while this is false.
	requested bool
	lock      sync.RWMutex
}

// extendDeadline pushes the deadline for the piece out to the given time, unless it already sits further out. The
// deadline only ever moves forward here because a peer choking us pushes it out to the far longer
// maxChokedDownloadWait grace: requests are pipelined, so a chunk asked for before a choke routinely lands just after
// it, and setting the ordinary deadline unconditionally there would throw away the partial piece that suspending the
// download exists to protect. The piece's lock must be held.
func (one *piece) extendDeadline(deadline time.Time) {
	if deadline.After(one.timeout) {
		one.timeout = deadline
	}
}

func newPeer(client *Client, conn net.Conn, logger *slog.Logger) *peer {
	now := time.Now()
	return &peer{
		client: client,
		logger: logger,
		conn:   conn,
		// A connection that has yet to send anything has been quiet since it was made, so that is where the wait for
		// the first keep-alive starts.
		lastWriteTime:        now,
		created:              now,
		has:                  fixedbits.New(client.torrentFile.PieceCount()),
		requestChan:          make(chan *pieceRequest),
		writeQueue:           make(chan []byte, 32),
		chokedChan:           make(chan struct{}, 1),
		stateChanged:         make(chan struct{}, 1),
		chunkRequestsChanged: make(chan struct{}, 1),
		pendingChunkRequests: make(map[int]bool),
		pieces:               make(map[int]*piece),
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
	// requestsUnsent is true while we hold a piece whose requests haven't reached the wire yet. A peer can't answer
	// what we haven't managed to ask it for, so it is excused from the stall check until they have.
	requestsUnsent bool
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

// downloadStalled returns true if no progress has been made on the current download within the allowed time. A peer
// whose requests are still queued up on our side has yet to be asked for anything, so it is never stalled: the write
// queue drains no faster than the rate limiter and the peer allow, and blaming a peer for our own backlog costs it
// both the connection and, since adjustPeers bans a stalled peer, the ability to come back.
func (s *peerState) downloadStalled(now time.Time) bool {
	if s.requestsUnsent {
		return false
	}
	return now.Sub(s.lastProgress()) > maxWaitForChunkDownload
}

// updateInterest recomputes whether we're interested in what this peer has, records it, and makes sure the peer gets
// told about any change. It is called from both the goroutine reading messages from the peer and the one managing
// peers, so amInterested is only ever recorded while the lock is held and the delivery of the message is left to
// processStateChanges. The whole of it is serialized as well, since the decision is made from a snapshot and recorded
// afterwards: two callers left to interleave can have the one that looked first record last, leaving us claiming an
// interest that contradicts what the peer has until something recomputes it, which may not be until the next peer
// adjustment 15 seconds later.
func (p *peer) updateInterest() peerState {
	p.clearExpiredDownloads()
	p.interestLock.Lock()
	defer p.interestLock.Unlock()
	p.lock.RLock()
	has := p.has.Clone()
	pieces := maps.Clone(p.pieces)
	p.lock.RUnlock()
	downloading := len(pieces) > 0
	// Whether anything has actually been asked for is read from the pieces themselves rather than tracked alongside
	// them, so that there is only one record of it to keep in step.
	requestsUnsent := false
	for _, one := range pieces {
		one.lock.RLock()
		requested := one.requested
		one.lock.RUnlock()
		if !requested {
			requestsUnsent = true
			break
		}
	}
	interested := downloading || p.client.tracker.isInteresting(has)
	p.lock.Lock()
	p.amInterested = interested
	changed := p.toldInterested != interested
	ps := p.peerState
	ps.downloading = downloading
	ps.requestsUnsent = requestsUnsent
	p.lock.Unlock()
	if changed {
		p.signalStateChange()
	}
	return ps
}

// setChoked records whether we're choking the peer, leaving the delivery of the message saying so to
// processStateChanges. Beginning to choke also tells the queue of outstanding piece requests to throw them away: BEP 3
// has the choked side treat everything it asked for as discarded, so serving those requests afterwards spends our
// upload bandwidth on data the remote counts as wasted, which is the opposite of what choking it was for.
func (p *peer) setChoked(choked bool) {
	p.lock.Lock()
	changed := p.amChoking != choked
	p.amChoking = choked
	p.lock.Unlock()
	if changed {
		p.signalStateChange()
		if choked {
			select {
			case p.chokedChan <- struct{}{}:
			default:
			}
		}
	}
}

// isChoking returns whether we're currently choking the peer, which is to say refusing to serve what it asks us for.
func (p *peer) isChoking() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.amChoking
}

// queueHave records that the peer has yet to be told that we now have the given piece, leaving the delivery of the
// message saying so to processStateChanges.
//
// A peer that is itself in the middle of downloading that very piece has just lost the endgame race for it. Its copy is
// worth nothing to us now, so the piece is dropped here and the remote is told to stop serving what we asked it for:
// left alone, it would go on sending the rest of the piece — every chunk of it landing in the branch of receivedChunk
// that ignores chunks for pieces we aren't downloading — while the one piece at a time this peer may hold kept it from
// taking on anything else until expiry reaped the record two minutes later. Nothing is given back to the tracker for
// it, since markBlockValid drops every owner of the index in the same critical section that sets the have bit, which is
// before any of this fan-out begins. Neither can the peer that completed the piece reach the abandonment at all:
// completePiece takes its own record of the piece out before markBlockValid runs.
//
// informPeersWeHavePiece is the only caller, and it runs on the read goroutine of the peer that completed the piece,
// walking every peer we have. So nothing here may wait on this peer's write queue, which for a peer we're uploading to
// under a cap is backed up for tens of seconds: recording what has to be sent and signaling the goroutine that owns the
// sending is the discipline the have alone already followed, and the cancels follow it too.
func (p *peer) queueHave(index int) {
	p.lock.Lock()
	// Reading the piece and dropping it happen in the one critical section, so no re-check of the kind
	// releaseIfStillOurs makes is needed: nothing can swap the entry for another between the two.
	one, abandoned := p.pieces[index]
	if abandoned {
		delete(p.pieces, index)
		delete(p.pendingChunkRequests, index)
	}
	p.lock.Unlock()
	var cancels [][]byte
	if abandoned {
		// Built with the peer's lock released, since the piece's own lock is what guards what was asked for and the
		// order between the two is the piece's first.
		cancels = cancelsForAbandonedPiece(index, one)
	}
	// The two are recorded together, and the have is not recorded before them, because nextStateMessage decides the
	// order it sends them in from whatever is queued at the moment it is asked: a delivery pass already under way for
	// an earlier signal would otherwise find the have alone and put it on the wire ahead of the cancels it is supposed
	// to follow.
	p.lock.Lock()
	p.cancelQueue = append(p.cancelQueue, cancels...)
	p.haveQueue = append(p.haveQueue, index)
	p.lock.Unlock()
	p.signalStateChange()
	if abandoned {
		// The peer is idle again, and this being the endgame there is a duplicate of somebody else's piece for it to
		// take, which nothing else would offer it: no piece was given back, so no peer is nudged, and a peer that has
		// already unchoked us and told us what it holds has nothing left to say that would start a download. Called
		// with no locks held, since it may cascade the endgame fan-out over every peer we have — on the completing
		// peer's read goroutine, and bounded there for the reasons startDownloadIfNeeded gives.
		p.startDownloadIfNeeded()
	}
}

// cancelsForAbandonedPiece builds the messages that take back what the peer was asked for on a piece we've given up on,
// which is exactly the blocks that were asked for and haven't been answered. The chunk aligned blocks are walked the
// way nextChunkRequests walks them, since those are the requests that actually went out: what lies at or beyond askedTo
// was never asked for, and what the spans cover has already arrived. A piece whose requests a choke discarded has
// askedTo wound back to the beginning, so it names nothing at all here, which is right — the remote was already told to
// throw those requests away.
//
// Only the piece's own lock is taken, and it is taken alone. The order this file keeps is the piece's lock before the
// peer's, and nothing here needs anything from the peer.
func cancelsForAbandonedPiece(index int, one *piece) [][]byte {
	one.lock.RLock()
	defer one.lock.RUnlock()
	var cancels [][]byte
	for begin := 0; begin < one.askedTo; begin += chunkSize {
		size := min(chunkSize, len(one.buffer)-begin)
		if !one.spans.Contains(&spanlist.Span{Start: begin, Length: size}) {
			cancels = append(cancels, newCancelMessage(index, begin, size))
		}
	}
	return cancels
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
	// The cancels for a piece somebody else finished go out ahead of the announcement that we have it. Both come from
	// the same abandonment, and a remote told first that we hold the piece and only then to stop serving it has every
	// reason to wonder what we were still asking for, while the other order says nothing surprising at all.
	case len(p.cancelQueue) != 0:
		buffer := p.cancelQueue[0]
		if len(p.cancelQueue) == 1 {
			p.cancelQueue = nil
		} else {
			p.cancelQueue = p.cancelQueue[1:]
		}
		return buffer
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

// queueChunkRequests records that the chunks we still need of the given piece have yet to be asked for, leaving the
// delivery of the messages that ask for them to processChunkRequests.
func (p *peer) queueChunkRequests(index int) {
	p.lock.Lock()
	p.pendingChunkRequests[index] = true
	p.lock.Unlock()
	p.signalChunkRequests()
}

// signalChunkRequests tells the goroutine that asks the peer for chunks that there may be something for it to send.
// The signal is never allowed to block, for the same reason signalStateChange's isn't: it says nothing more than that
// there is something to look at, and the delivery goroutine re-reads everything pending each time it wakes.
func (p *peer) signalChunkRequests() {
	select {
	case p.chunkRequestsChanged <- struct{}{}:
	default:
	}
}

// processChunkRequests asks the peer for the chunks it hasn't delivered yet of the pieces we're downloading from it. A
// goroutine of its own owns those sends because a piece is a whole queue's worth of request messages and the write
// queue blocks once it fills, which for a peer whose queue is backed up behind rate limited piece messages is tens of
// seconds. The pieces are claimed by the peer management goroutine and by the read goroutines of other peers, so a
// send made where the claim is decided would stall choke rotation, expiry clearing and dialing, or another peer's
// message loop, on whichever peer happens to be the slowest.
func (p *peer) processChunkRequests(done chan bool) {
	for {
		select {
		case <-p.chunkRequestsChanged:
			for _, buffer := range p.nextChunkRequests() {
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

// nextChunkRequests returns the messages asking for as much as may be asked for right now of the chunks we still need
// of the pieces that have been queued for requesting, and takes the pieces that have nothing left to ask for out of
// the queue. The messages are built when they are about to be sent rather than when the piece was queued, so chunks
// that arrived while an earlier batch was draining aren't asked for a second time, and a piece that was given back in
// the meantime isn't asked for at all.
//
// No more than maxOutstandingChunkRequests are ever left in front of a peer at once, so a piece whose chunks run past
// that is asked for a batch at a time as the answers come back rather than all in one burst. A piece that stays in the
// queue with chunks left to ask for is picked up again by the signal receivedChunk makes when a chunk arrives, which
// is what frees the room for the next batch.
func (p *peer) nextChunkRequests() [][]byte {
	p.lock.RLock()
	indexes := make([]int, 0, len(p.pendingChunkRequests))
	for index := range p.pendingChunkRequests {
		indexes = append(indexes, index)
	}
	pieces := maps.Clone(p.pieces)
	p.lock.RUnlock()
	slices.Sort(indexes) // Map iteration order is arbitrary; the peer is asked for the pieces in a sensible one
	// What may be asked for is what the cap leaves once everything already asked for is accounted for. The count is
	// derived from the pieces themselves rather than tracked alongside them so that it cannot drift: a piece that
	// completed, or that was given back because it stalled, takes whatever it had outstanding with it.
	budget := maxOutstandingChunkRequests
	for _, one := range pieces {
		one.lock.RLock()
		budget -= one.inFlight
		one.lock.RUnlock()
	}
	var requests [][]byte
	settled := make(map[int]*piece, len(indexes))
	for _, index := range indexes {
		one, ok := pieces[index]
		if !ok {
			// The piece finished, or was given back, while it sat in the queue, so there is nothing left to ask for
			settled[index] = nil
			continue
		}
		one.lock.Lock()
		for budget > 0 && one.askedTo < len(one.buffer) {
			size := min(len(one.buffer)-one.askedTo, chunkSize)
			if !one.spans.Contains(&spanlist.Span{Start: one.askedTo, Length: size}) {
				requests = append(requests, newRequestMessage(index, one.askedTo, size))
				one.inFlight++
				budget--
			}
			one.askedTo += size
		}
		if one.askedTo >= len(one.buffer) {
			settled[index] = one
		}
		one.lock.Unlock()
	}
	p.dropSettledChunkRequests(settled)
	return requests
}

// dropSettledChunkRequests takes out of the queue of pieces to ask for those that have nothing left to ask for,
// which is what the caller decided from a snapshot taken before the lock was released. Each entry names the piece the
// decision was made about, with a nil piece for one that was already gone, and only an index whose claim is still that
// same piece is dropped.
//
// The re-check is what keeps a piece re-claimed in that window from being silently dropped: queuePieceDownload records
// the fresh claim and queues it, and deleting the entry the caller saw would take that queuing with it, leaving the
// piece's chunks never asked for and the claim to sit idle until maxDownloadStall releases it two minutes later. A
// piece has every chance to be re-claimed there, since it becomes available the moment it is given back — by
// clearExpiredDownloads, which removes the piece but leaves the queue entry behind, or by the completion that takes it
// out of the map.
func (p *peer) dropSettledChunkRequests(settled map[int]*piece) {
	if len(settled) == 0 {
		return
	}
	p.lock.Lock()
	for index, one := range settled {
		if p.pieces[index] == one {
			delete(p.pendingChunkRequests, index)
		}
	}
	p.lock.Unlock()
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
	buffer := make([]byte, requestMessageLength)
	binary.BigEndian.PutUint32(buffer[:4], 13)
	buffer[4] = requestID
	binary.BigEndian.PutUint32(buffer[5:9], uint32(index))
	binary.BigEndian.PutUint32(buffer[9:13], uint32(begin))
	binary.BigEndian.PutUint32(buffer[13:], uint32(length))
	return buffer
}

// newCancelMessage creates the message that tells a peer we no longer want a chunk we asked it for. Its layout is the
// request message's down to the length, the ID alone telling the two apart, which is what noteRequestSent keys on: a
// cancel going out must not start the clocks that decide whether the peer is still delivering a piece, since the piece
// it names is precisely one we've stopped waiting for.
func newCancelMessage(index, begin, length int) []byte {
	buffer := make([]byte, requestMessageLength)
	binary.BigEndian.PutUint32(buffer[:4], 13)
	buffer[4] = cancelID
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

// rejectPeer blocks the peer's address and closes the connection, which is what a peer that has broken the protocol
// gets. Closing it is not something the caller can leave to the read loop's teardown: that teardown first waits for a
// slot on the write queue, and a queue full of piece messages draining under an upload cap doesn't yield one for tens
// of seconds — during which a peer we've just decided is hostile goes on holding one of our limited peer slots and
// receiving everything we had queued up for it.
func (p *peer) rejectPeer() {
	p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
	xio.CloseIgnoringErrors(p.conn)
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
				p.startDownloadsOnOtherPeers()
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
	go p.processChunkRequests(done)
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
			p.rejectPeer()
			return
		}
		if length > 0 { // Not a keep-alive message
			buffer := make([]byte, length)
			if err := tio.ReadWithDeadline(p.conn, buffer, messageReadDeadline(length)); err != nil {
				if tio.ShouldLogIOError(err) {
					errs.LogTo(p.logger, err)
				}
				p.blockAddressOnReadFailure(err)
				return
			}
			if !validMessageLength(buffer[0], length) {
				p.logger.Warn("invalid message length", "id", int(buffer[0]), "length", length)
				p.rejectPeer()
				return
			}
			switch buffer[0] {
			case chokeID:
				p.lock.Lock()
				changed := !p.peerChoking
				p.peerChoking = true
				p.lock.Unlock()
				// Only an actual transition may act on the choke. Suspending pushes the deadline for every piece we're
				// in the middle of downloading out to maxChokedDownloadWait, so a remote that merely re-sends choke
				// often enough would otherwise keep a piece it never delivers claimed indefinitely: nothing could
				// expire it, no other peer could take it, and the peer is excused from the stall check for as long as
				// it is choking us.
				if changed {
					p.suspendDownloads()
				}
			case unchokeID:
				p.lock.Lock()
				changed := p.peerChoking
				p.peerChoking = false
				p.lock.Unlock()
				// As with the choke above, only an actual transition may act on it. Resuming restarts the deadline for
				// every piece we hold, restarts the clock the stall check measures from, and asks again for every chunk
				// that hasn't arrived, so a duplicate unchoke is both a way to hold a piece without delivering any of
				// it and a way to have us send far more than the five bytes it cost to ask.
				if changed {
					p.resumeDownloads()
				}
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
				// The index is unverified data from the peer, and on a platform where int is 32 bits a large enough
				// one arrives as a negative number, so the sign is checked in its own right. A have naming a piece the
				// torrent doesn't contain is a protocol violation like every other malformed message this loop
				// refuses: Set ignores an index it can't hold, so letting one through would leave a broken peer looking
				// healthy while it could never be interesting to us.
				index := int(binary.BigEndian.Uint32(buffer[1:]))
				if index < 0 || index >= p.client.torrentFile.PieceCount() {
					p.logger.Warn("have for a piece outside the torrent", "index", index,
						"pieces", p.client.torrentFile.PieceCount())
					p.rejectPeer()
					return
				}
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
					p.rejectPeer()
					return
				}
				// BEP 3 requires the bits past the end of the field to be zero and has a downloader drop a peer that
				// sets them, which is a peer claiming pieces the torrent doesn't contain. SetBytes normalizes them
				// away, so refusing them has to happen before the field is taken in — and the check the length gets
				// just above says nothing about the content it admits.
				if p.has.SpareBitsSet(buffer[1:]) {
					p.lock.Unlock()
					p.logger.Warn("bit field with spare bits set", "pieces", p.client.torrentFile.PieceCount())
					p.rejectPeer()
					return
				}
				p.has.SetBytes(buffer[1:])
				p.lock.Unlock()
				p.updateInterest()
				// A download is started here for the same reason the have above starts one: this is one of the two ways
				// we learn what a peer has, and nothing else would. A peer that unchoked us before telling us what it
				// held has no piece claimed and no further nudge coming — adjustPeers and updateInterest never start a
				// download, and startDownloadsOnOtherPeers only fires when some piece is given back — so in a small
				// swarm we would sit interested and idle against it for as long as the torrent ran.
				p.startDownloadIfNeeded()
			case requestID:
				req := newPieceRequest(buffer, false)
				if !p.validPieceRequest(req) {
					p.logger.Warn("invalid piece request", "index", req.index, "begin", req.begin, "length", req.length)
					p.rejectPeer()
					return
				}
				if !p.isChoking() {
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
		one.lock.Lock()
		one.timeout = deadline
		// The requests we're about to build have yet to go out, so the deadline is inert again until they do, the same
		// as it was when the piece was first claimed.
		one.requested = false
		// A peer that was choking us was told to discard everything it had been asked for, so nothing is in flight any
		// longer and what is still missing has to be asked for again from the beginning of the piece.
		one.askedTo = 0
		one.inFlight = 0
		one.lock.Unlock()
		p.queueChunkRequests(index)
	}
}

// startDownloadsOnOtherPeers tells every peer but this one to look for something to download. It is called whenever
// this peer gives a piece back, since nothing else nudges a peer that is already connected, unchoked and idle: neither
// adjustPeers nor updateInterest starts a download, so in a small or static swarm the freed piece would otherwise sit
// unclaimed until some peer happened to disconnect or to unchoke us.
func (p *peer) startDownloadsOnOtherPeers() {
	for _, other := range p.client.currentPeers() {
		if other != p {
			other.startDownloadIfNeeded()
		}
	}
}

// startDownloadIfNeeded claims a piece for this peer to work on, unless it already holds one, is being choked, or is
// on its way out. The claiming is serialized, since whether the peer may take one on is decided from a snapshot of
// what it holds and acted on afterwards, with the tracker's lock taken in between: two callers left to interleave both
// find it holding nothing and both claim, leaving it with two pieces, a single maxOutstandingChunkRequests budget that
// the first can consume entirely, and a second piece that is never asked for and that no other peer can take until
// maxDownloadStall gives it back two minutes later.
//
// Each pass around the loop re-reads what the peer holds, so a bail or a choke that lands in the middle of it is
// honored rather than run past. It goes around only when queuePieceDownload reports that the piece it was handed was
// given straight back because the torrent already holds it, which leaves the peer as idle as it was before and is
// worth one more look. That can happen at most once per piece, since a piece we have is one nothing hands out again.
func (p *peer) startDownloadIfNeeded() {
	p.downloadStartLock.Lock()
	claimed := false
	for {
		var has *fixedbits.Bits
		p.lock.RLock()
		if !p.bail && !p.peerChoking && len(p.pieces) == 0 {
			has = p.has.Clone()
		}
		p.lock.RUnlock()
		if has == nil {
			break
		}
		index := p.client.tracker.selectForDownloading(p, has)
		if index == -1 {
			break
		}
		if p.queuePieceDownload(index) {
			claimed = true
			break
		}
	}
	p.downloadStartLock.Unlock()
	// A fresh claim that takes the last piece nothing was fetching is the moment the endgame begins, and that moment
	// has to be carried to the other peers: they are already connected, unchoked and idle, and in a swarm of seeders no
	// have or unchoke is coming that would wake them, so the peers the endgame exists to recruit would never be asked.
	// Releases nudge the same way, which is why only claims are handled here. A claim made once the endgame is already
	// under way re-fires the fan-out harmlessly: every peer that could still duplicate anything is holding a piece by
	// then and drops out at the gate above.
	//
	// The check is made after the serialization lock is released because startDownloadsOnOtherPeers reaches
	// startDownloadIfNeeded for every other peer on this same goroutine, and through their own nudges possibly back
	// into this one, and the lock is not reentrant. The cascade is finite for the same reason it is harmless: a peer
	// that already holds a piece claims nothing and so nudges no one, which leaves each peer at most one claim, and one
	// nudge, per cascade.
	//
	// Asking the tracker after the claim rather than deciding it as part of the claim is benignly racy: a release
	// landing in between hides the endgame from this claimant, and costs nothing, since every path that releases a
	// piece runs startDownloadsOnOtherPeers itself. Outside the endgame the whole of the cost is one read lock on the
	// tracker per successful claim.
	if claimed && p.client.tracker.inEndgame() {
		p.startDownloadsOnOtherPeers()
	}
}

// queuePieceDownload records the piece the tracker has handed this peer and queues up the requests for what we still
// need of it, reporting whether the peer has anything to do as a result. False is answered for exactly one outcome:
// the piece was given straight back because the torrent turns out to already hold it, which leaves the peer idle and
// is the only case in which looking for another piece makes sense. Everything else answers true, the piece taken on
// along with the two that end the search — a peer on its way out, which is going nowhere, and an index the peer was
// already downloading, which is the piece it is working on.
func (p *peer) queuePieceDownload(index int) bool {
	length := int(p.client.torrentFile.LengthOf(index))
	p.lock.Lock()
	// The piece has to be given back if we're already on our way out. Our teardown releases everything it finds in the
	// map and then leaves for good, so a piece recorded after that would be left marked as being downloaded by a peer
	// that no longer exists, with nothing to ever release it and no other peer able to take it.
	bailing := p.bail
	one, ok := p.pieces[index]
	if !bailing && !ok {
		now := time.Now()
		// The deadline recorded here doesn't start running until the requests for the piece actually reach the wire,
		// which processWriteQueue is what knows. Claiming a piece only puts it on the queue of things to ask for, and
		// that queue is drained by a goroutine that has to wait its turn behind everything else we owe the peer.
		one = &piece{
			buffer:       make([]byte, length),
			timeout:      now.Add(downloadReadDeadline),
			lastProgress: now,
		}
		p.pieces[index] = one
		p.downloadStarted = now
	}
	p.lock.Unlock()
	if bailing {
		p.client.tracker.clearDownload(index, p)
		return true
	}
	if ok {
		return true
	}
	// The claim was registered under the tracker's lock before this peer had any record of the piece, so a completion
	// that landed in that window — another peer's markBlockValid, which sets the have bit and drops every claim on the
	// index in the same critical section — leaves this peer set to pull a whole piece of data we already hold off the
	// network. Recording the piece and asking afterwards is what closes that: the common case, where the completion is
	// already in, is caught here before a single request has gone out, and a completion this check does miss must have
	// landed after the record existed rather than in the blind window before it. Missing one costs a piece fetched
	// twice, which is what the endgame deliberately arranges anyway, and nothing more: the data validates and a repeat
	// markBlockValid says nothing.
	if p.client.tracker.hasPiece(index) {
		p.lock.Lock()
		// Only the very piece just recorded is taken back out, which is the re-check releaseIfStillOurs makes and for
		// the same reason: the index may have been given back and claimed again for a piece of its own while the
		// tracker was being asked, and dropping whatever is there would throw that away.
		if p.pieces[index] == one {
			delete(p.pieces, index)
		}
		p.lock.Unlock()
		// Usually nothing is left to give back, the winner having dropped every claim on the index as it set the have
		// bit, but what this peer registered is this peer's to release and nothing else would do it.
		p.client.tracker.clearDownload(index, p)
		return false
	}
	p.queueChunkRequests(index)
	return true
}

func (p *peer) receivedChunk(index, begin int, buffer []byte) error {
	p.lock.RLock()
	one, ok := p.pieces[index]
	p.lock.RUnlock()
	if !ok {
		// A chunk for a piece we aren't downloading is dropped rather than treated as an offense, since we cause the
		// duplicates ourselves: the requests of a batch made before the peer choked us go out after it unchokes us,
		// alongside the fresh batch resumeDownloads builds, so an honest peer serves the same chunk twice and the
		// second copy arrives after the first has completed the piece and taken it out of the map. A lenient peer that
		// serves requests we released across a choke does the same. The bytes are charged to the download rate by the
		// read loop either way.
		p.logger.Debug("ignoring a chunk for a piece we aren't downloading", "index", index, "begin", begin)
		return nil
	}
	// The offset is unverified data from the peer. On a platform where int is 32 bits a large enough one arrives as a
	// negative number, which would make the range that is checked below smaller than the one that is actually written,
	// so the sign is checked in its own right and the end of the range is computed without being able to overflow.
	if begin < 0 || len(buffer) > len(one.buffer) || begin > len(one.buffer)-len(buffer) {
		p.rejectPeer()
		return errs.Newf("piece %d would overrun buffer", index)
	}
	// The chunk has to be one of the chunk aligned blocks we actually asked for, which is the only thing a peer is
	// supposed to answer a request with. Taking whatever range a peer cares to send is what lets it defeat the bounds
	// on how long it may hold a piece: every byte we haven't seen before counts as progress, so a peer that dribbles
	// out a single new byte at a time renews both deadlines indefinitely while delivering nothing anyone can use, and
	// no other peer can take the piece while it is claimed. It is also what makes taking a chunk in cost more than the
	// chunk is worth: single bytes at every other offset build a span for each one, and both the search and the
	// insertion that records a span are linear in the spans already there, so a peer can spend our CPU quadratically
	// for as long as it keeps feeding us.
	if begin%chunkSize != 0 || len(buffer) != min(chunkSize, len(one.buffer)-begin) {
		p.rejectPeer()
		return errs.Newf("piece %d chunk at %d with a length of %d was never requested", index, begin, len(buffer))
	}
	last := begin + len(buffer)
	span := spanlist.Span{Start: begin, Length: len(buffer)}
	one.lock.Lock()
	now := time.Now()
	bailIfNotFinish := one.requested && one.timeout.Before(now)
	// Data coming back is proof that what we asked for reached the peer, whether or not the goroutine that put the
	// requests on the wire has caught up with recording it.
	one.requested = true
	// A chunk that carries nothing we don't already have makes no progress, so it must not renew either of the
	// deadlines that decide whether this peer is still delivering the piece it was asked for. A peer that dribbles out
	// duplicate chunks would otherwise hold onto a piece indefinitely, at almost no cost to itself, and no other peer
	// could take it.
	progressed := !one.spans.Contains(&span)
	if progressed {
		one.extendDeadline(now.Add(downloadReadDeadline))
		one.lastProgress = now
		// The request this answers is no longer in front of the peer, so it stops counting against the cap on how many
		// of them we keep there. A duplicate is not counted off: the request it answers was already accounted for by
		// the copy that arrived first, or was discarded along with the rest when the peer choked us.
		if one.inFlight > 0 {
			one.inFlight--
		}
	}
	copy(one.buffer[begin:last], buffer)
	one.spans.Insert(&span)
	if progressed {
		p.lock.Lock()
		p.lastReceived = now
		p.lock.Unlock()
	}
	if len(one.spans.Spans) == 1 && one.spans.Spans[0].Start == 0 && one.spans.Spans[0].Length == len(one.buffer) {
		// The piece is whole, so nothing writes to its buffer any more and the reference alone is all that finishing
		// it needs. The lock is dropped here rather than after that work for the reason completePiece gives.
		whole := one.buffer
		one.lock.Unlock()
		return p.completePiece(index, one, whole)
	}
	one.lock.Unlock()
	if bailIfNotFinish {
		p.bailOut()
	} else {
		// The chunk that just arrived freed room under the cap on how many requests we keep in front of a peer, so
		// this is what carries the rest of a piece too large to ask for in one go out to it, a batch at a time.
		p.signalChunkRequests()
	}
	return nil
}

// completePiece verifies a piece whose every chunk has arrived, writes it to storage and gives up the claim on it. The
// buffer is handed in rather than read back out of the piece because the piece's lock is deliberately not held for any
// of this: a full-piece SHA-1 and a write of up to tfs.MaxPieceLength both run here, and that lock is taken for every
// piece of every peer by clearExpiredDownloads, which the peer management goroutine reaches from its expiry tick and
// from adjustPeers. Holding it across them stalls choke rotation, expiry clearing and dialing for every peer at once,
// along with Client.Stop's bounded wait for peer management and this peer's own processChunkRequests.
func (p *peer) completePiece(index int, one *piece, buffer []byte) error {
	if !p.client.torrentFile.Validate(index, buffer) {
		p.rejectPeer()
		return errs.Newf("discarding invalid piece %d", index)
	}
	var n int
	err := errStorageClosed
	if f := p.client.storageFile(); f != nil {
		n, err = f.WriteAt(buffer, p.client.torrentFile.OffsetOf(index))
	}
	// Only the claim this very piece holds is given up, which is the same re-check releaseIfStillOurs makes and for the
	// same reason. Nothing has held the piece's lock since the buffer was taken, so clearExpiredDownloads may have
	// released the index in the meantime and another goroutine re-claimed it here for a piece of its own — it is
	// available again, since the block isn't marked valid until further down.
	//
	// A fresh claim on this index is on its way out either way, since the announcement further down abandons every
	// record of a piece we now hold, but which of the two drops it decides what the peer is left owing us. Deleting
	// whatever is there loses the requests already on the wire along with it, and they are never taken back: the peer
	// answers with a piece we have just marked as had, every chunk of it landing in the branch of receivedChunk that
	// ignores chunks for pieces we aren't downloading, and the whole of it charged to the tracker's downloaded total.
	// It is also what the storage failure below would give back, handing away an index that belongs to whoever took it
	// up rather than to the write that failed.
	p.lock.Lock()
	if p.pieces[index] == one {
		delete(p.pieces, index)
	}
	p.lock.Unlock()
	if err != nil && (!errors.Is(err, io.EOF) || n != len(buffer)) {
		p.client.tracker.clearDownload(index, p)
		writeErr := errs.NewWithCause("unable to write piece", err)
		// As on the read side, storage the shutdown has already closed isn't a failure to write
		if !storageClosed(err) {
			errs.LogTo(p.logger, writeErr, "index", index)
		}
		// Nothing a peer can do fixes storage we can't write to, so the piece is not simply downloaded again: doing
		// that would have us pull whole pieces off the network, forever, for as long as the disk stayed full or the
		// file unwritable, making no progress and saying nothing about it beyond another log line each time around.
		// The torrent is stopped with the error instead.
		p.client.failWithStorageError(writeErr)
		return nil
	}
	p.client.tracker.markBlockValid(index)
	p.client.tracker.setProgress(-1)
	if p.client.tracker.isDownloadComplete() {
		p.client.tracker.setState(Seeding)
	}
	p.updateInterest()
	p.startDownloadIfNeeded()
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
			select {
			case req, ok = <-p.requestChan:
				if !ok {
					return
				}
			case <-p.chokedChan:
				// Nothing is being held, so there is nothing to discard, but the signal still has to be taken out of
				// the way of the one that a later choke will send.
				continue
			}
		} else {
			select {
			case req, ok = <-p.requestChan:
				if !ok {
					// The requests still in hand are dropped rather than fulfilled. This only runs as the peer session
					// ends, so every response built here is discarded by the write queue's drain or fails on a dead
					// connection, and up to maxPendingPieceRequests of them is some 8MB of storage read for nothing —
					// which a peer that gets itself disconnected for flooding us with requests can ask for over and
					// over, at the cost of 13KB of request traffic each time.
					return
				}
			case <-p.chokedChan:
				// The state is re-read rather than trusted to the signal, which may have been sent for a choke that
				// has since been lifted: discarding requests that arrived after the unchoke would leave the peer
				// waiting on data that is never coming and never asked for again.
				if p.isChoking() {
					queue = newPendingRequests()
				}
				continue
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
		if p.isChoking() {
			// The peer was told to treat everything it had asked us for as discarded, so a request that was already
			// on its way through here when we began choking is dropped rather than served.
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
			readErr := errs.NewWithCause("unable to read piece", err)
			// Storage the shutdown has already closed isn't a failure to read, so it isn't reported as one. This
			// goroutine isn't tracked by the peer wait group, so it can be here with a descriptor it fetched a moment
			// before the shutdown closed it, which reads back as os.ErrClosed.
			if !storageClosed(err) {
				errs.LogTo(p.logger, readErr, "index", req.index, "begin", req.begin, "length", req.length)
			}
			xio.CloseIgnoringErrors(p.conn)
			process = false
			// Nothing a peer can do fixes storage we can't read, so this can't be left as one connection's problem:
			// every other peer asking for data runs into the same failure and is dropped for it, leaving a torrent
			// that goes on running as a seeder which serves no one, with nothing said about it beyond a log line per
			// peer. The torrent is stopped with the error instead, the same way an unwritable storage stops it.
			// Storage the shutdown has already closed is not a failure and is filtered out by failWithStorageError.
			p.client.failWithStorageError(readErr)
			continue
		}
		p.writeQueue <- buffer
	}
}

// noteRequestSent starts the clocks that decide whether the peer is still delivering the piece that the message about
// to be written asks for, and does nothing for any other message. They are started here rather than where the piece
// was claimed because the two can be tens of seconds apart: the requests for a piece are a whole batch of messages
// that have to wait their turn on the write queue behind whatever we already owe the peer, which for one we're also
// uploading to under a cap drains slowly. A deadline anchored at the claim expires while the requests are still
// queued, and clearExpiredDownloads then releases the piece and — the peer isn't choking us, since it has no reason
// to be — gives up on a peer that was never asked for anything, which adjustPeers finishes off by banning its address.
func (p *peer) noteRequestSent(buffer []byte) {
	if len(buffer) != requestMessageLength || buffer[4] != requestID {
		return
	}
	p.lock.RLock()
	one, ok := p.pieces[int(binary.BigEndian.Uint32(buffer[5:9]))]
	p.lock.RUnlock()
	if !ok {
		return
	}
	now := time.Now()
	one.lock.Lock()
	first := !one.requested
	one.requested = true
	one.extendDeadline(now.Add(downloadReadDeadline))
	one.lock.Unlock()
	// Only the first request of a batch moves the stall clock, which is the peer's rather than the piece's: the rest
	// of the batch goes out behind it, and letting each one push the clock along would excuse a peer for as long as we
	// were still asking.
	if first {
		p.lock.Lock()
		if p.downloadStarted.Before(now) {
			p.downloadStarted = now
		}
		p.lock.Unlock()
	}
}

func (p *peer) processWriteQueue(done chan bool) {
	go p.keepAlive(done)
	for buffer := range p.writeQueue {
		var err error
		if buffer != nil {
			if len(buffer) == 4 {
				// A keep-alive that finds the wire hasn't actually been quiet is dropped, since anything else we sent
				// within the last keepAlivePeriod has already told the remote we're still here. The goroutine that
				// queues them measures from that write rather than from this attempt, so nothing is pushed out.
				if time.Since(p.lastWrite()) < keepAlivePeriod {
					continue
				}
			} else {
				err = useRate(p.client.OutRate, len(buffer))
			}
			if err == nil {
				// The clocks that decide whether the peer is still delivering a piece are started here, where the
				// request for it is finally on its way out, rather than where the piece was claimed.
				p.noteRequestSent(buffer)
				p.setLastWrite(time.Now())
				err = tio.WriteWithDeadline(p.conn, buffer, messageWriteDeadline(len(buffer)))
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

// keepAlive queues a keep-alive whenever the connection is about to have been quiet for a full keepAlivePeriod. The
// wait runs from the last thing we actually wrote rather than from the previous attempt, because processWriteQueue
// drops a keep-alive that a recent write has already made unnecessary: measured from the attempt, a write early in one
// period would suppress the keep-alive at the end of it and leave the next one a full period further out, stretching
// the silence to nearly 2×keepAlivePeriod. That is past the 2m30s we ourselves are willing to wait for a message, and
// past the two minutes typical clients allow, so an idle but perfectly healthy connection gets dropped — and by a
// remote running this same library, blocked for five minutes on top of it, since a read timeout is neither EOF nor a
// connection we closed.
func (p *peer) keepAlive(done chan bool) {
	for {
		select {
		case <-time.After(p.keepAliveDelay(time.Now())):
			select {
			case p.writeQueue <- make([]byte, 4):
			case <-done:
				return
			}
		case <-done:
			return
		}
	}
}

// keepAliveDelay returns how long is left before the connection will have been quiet for keepAlivePeriod, which is
// when the next keep-alive is due. Zero means one is due now.
func (p *peer) keepAliveDelay(now time.Time) time.Duration {
	if delay := keepAlivePeriod - now.Sub(p.lastWrite()); delay > 0 {
		return delay
	}
	return 0
}

// lastWrite returns when we last put something on the wire for this peer.
func (p *peer) lastWrite() time.Time {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.lastWriteTime
}

// setLastWrite records that something is going out on the wire for this peer now.
func (p *peer) setLastWrite(when time.Time) {
	p.lock.Lock()
	p.lastWriteTime = when
	p.lock.Unlock()
}

func (p *peer) clearExpiredDownloads() {
	p.lock.RLock()
	m := maps.Clone(p.pieces)
	choking := p.peerChoking
	p.lock.RUnlock()
	now := time.Now()
	giveUpOnPeer := false
	released := false
	for k, v := range m {
		v.lock.RLock()
		// The deadline is what the ordinary cases turn on, but it is pushed out whenever the peer chokes or unchokes
		// us, so on its own it can be kept ahead of the clock by a remote that alternates the two without ever
		// delivering anything. The time since the piece last saw progress can't be, so it bounds the whole affair —
		// and it is the only bound while the requests for the piece are still waiting to go out, since until they do
		// the peer hasn't been asked for anything.
		requested := v.requested
		remove := (requested && v.timeout.Before(now)) || now.Sub(v.lastProgress) > maxDownloadStall
		v.lock.RUnlock()
		if !remove {
			continue
		}
		if p.releaseIfStillOurs(k, v) {
			released = true
			// A peer that is choking us was told to discard our requests, so having nothing to show for the piece
			// isn't its fault: give up the piece so another peer can take it, but keep the connection. Neither is a
			// piece whose requests never made it out of our own write queue.
			giveUpOnPeer = giveUpOnPeer || (!choking && requested)
		}
	}
	if released {
		p.startDownloadsOnOtherPeers()
	}
	if giveUpOnPeer {
		p.bailOut()
	}
}

// releaseIfStillOurs gives the index back, both here and to the tracker, but only if it is still claimed by the very
// piece the caller decided about, and reports whether it was. The re-check is the same one dropSettledChunkRequests
// makes, and for the same reason: the caller decided from a snapshot taken before the lock was released, so the index
// may name a piece our own teardown has already given up, one that finished while it was being looked at, or one
// another peer has claimed since, and releasing any of those takes the piece away from whoever is actually downloading
// it.
//
// Presence alone isn't enough. clearExpiredDownloads runs concurrently with itself as a matter of course — the peer
// management tick, the adjustPeers pass every fifteen seconds and the read goroutine's handling of every have and
// bitfield message all reach it — so two passes can hold a snapshot of the same expired piece at once. Once the first
// has released it and the index has been re-claimed for a fresh piece, a second pass that only asked whether something
// was there would throw that fresh piece and its partial download away, clear a tracker claim that isn't the one it
// decided about, and, with a stale requested flag behind it, close the connection of a peer that had just unchoked us.
func (p *peer) releaseIfStillOurs(index int, one *piece) bool {
	p.lock.Lock()
	ours := p.pieces[index] == one
	if ours {
		delete(p.pieces, index)
	}
	p.lock.Unlock()
	if ours {
		p.client.tracker.clearDownload(index, p)
	}
	return ours
}
