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
	chunkSize               = dispatcher.ChunkSize
	// maxMessageLength is the largest message we'll accept from a peer, not counting the length prefix. The largest
	// message we ever expect is a piece message, which is 9 bytes plus a chunk, but bit field messages grow with the
	// number of pieces in the torrent, so allow a generous amount beyond that.
	maxMessageLength = 128 * 1024
	// maxPendingPieceRequests is the number of unfulfilled piece requests we'll hold onto for a peer before deciding
	// it is flooding us.
	maxPendingPieceRequests = 512
)

// errStorageClosed is returned when a peer tries to read or write the torrent's storage after the client has closed
// it, which can happen for the peer goroutines that outlive the client's peer wait group.
var errStorageClosed = errors.New("torrent storage has been closed")

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
	pieces      map[int]*piece // protected by lock
	peerState                  // protected by lock
	bail        bool           // protected by lock
	lock        sync.RWMutex
}

type pieceRequest struct {
	index  int
	begin  int
	length int
	cancel bool
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
		return length >= 9
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
		client:      client,
		logger:      logger,
		conn:        conn,
		created:     time.Now(),
		has:         fixedbits.New(client.torrentFile.PieceCount()),
		requestChan: make(chan *pieceRequest),
		writeQueue:  make(chan []byte, 32),
		pieces:      make(map[int]*piece),
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

func (p *peer) updateInterest() peerState {
	p.clearExpiredDownloads()
	p.lock.RLock()
	has := p.has.Clone()
	ps := p.peerState
	ps.downloading = len(p.pieces) > 0
	p.lock.RUnlock()
	interested := ps.downloading || p.client.tracker.isInteresting(has)
	if ps.amInterested != interested {
		ps.amInterested = interested
		p.lock.Lock()
		p.amInterested = interested
		p.lock.Unlock()
		buffer := make([]byte, 5)
		binary.BigEndian.PutUint32(buffer[:4], 1)
		if interested {
			buffer[4] = interestedID
		} else {
			buffer[4] = notInterestedID
		}
		p.writeQueue <- buffer
	}
	return ps
}

func (p *peer) setChoked(choked bool) {
	p.lock.Lock()
	send := p.amChoking != choked //nolint:ifshort // incorrect assumption that send isn't used later
	if send {
		p.amChoking = choked
	}
	p.lock.Unlock()
	if send {
		buffer := make([]byte, 5)
		binary.BigEndian.PutUint32(buffer[:4], 1)
		if choked {
			buffer[4] = chokeID
		} else {
			buffer[4] = unchokeID
		}
		p.writeQueue <- buffer
	}
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
				p.client.tracker.clearDownload(index)
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
	go p.processWriteQueue()
	go p.pieceRequestQueue()
	lengthBuffer := make([]byte, 4)
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
			p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
			return
		}
		length := binary.BigEndian.Uint32(lengthBuffer)
		if length > maxMessageLength {
			p.logger.Warn("message too large", "length", length, "max", maxMessageLength)
			p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
			return
		}
		if length > 0 { // Not a keep-alive message
			buffer := make([]byte, length)
			if err := tio.ReadWithDeadline(p.conn, buffer, msgReadDeadline); err != nil {
				if tio.ShouldLogIOError(err) {
					errs.LogTo(p.logger, err)
				}
				p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
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
			case unchokeID:
				p.lock.Lock()
				p.peerChoking = false
				p.lock.Unlock()
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
		if err := <-p.client.InRate.Use(int(length + 4)); err != nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(p.logger, err)
			}
			return
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
	// Likely need to mark when this was requested and if it goes for too long,
	// remove from the list of downloading pieces.
	length := int(p.client.torrentFile.LengthOf(index))
	p.lock.Lock()
	_, ok := p.pieces[index]
	if !ok {
		now := time.Now()
		p.pieces[index] = &piece{
			buffer:  make([]byte, length),
			timeout: now.Add(downloadReadDeadline),
		}
		p.downloadStarted = now
	}
	p.lock.Unlock()
	if !ok {
		for i := 0; i < length; i += chunkSize {
			buffer := make([]byte, 17)
			binary.BigEndian.PutUint32(buffer[:4], 13)
			buffer[4] = requestID
			binary.BigEndian.PutUint32(buffer[5:9], uint32(index))
			binary.BigEndian.PutUint32(buffer[9:13], uint32(i))
			size := min(length-i, chunkSize)
			binary.BigEndian.PutUint32(buffer[13:], uint32(size))
			p.writeQueue <- buffer
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
	last := begin + len(buffer)
	if last > len(one.buffer) {
		p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
		return errs.Newf("piece %d would overrun buffer", index)
	}
	one.lock.Lock()
	now := time.Now()
	bailIfNotFinish := one.timeout.Before(now)
	one.timeout = now.Add(downloadReadDeadline)
	copy(one.buffer[begin:last], buffer)
	one.spans.Insert(&spanlist.Span{Start: begin, Length: len(buffer)})
	p.lock.Lock()
	p.lastReceived = now
	p.lock.Unlock()
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
				p.client.tracker.clearDownload(index)
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
			p.lock.Lock()
			p.bail = true
			p.lock.Unlock()
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

func (p *peer) processWriteQueue() {
	var lastWriteTime time.Time
	done := make(chan bool)
	go p.keepAlive(done)
	for buffer := range p.writeQueue {
		var err error
		if buffer != nil {
			if len(buffer) == 4 {
				if time.Since(lastWriteTime) < keepAlivePeriod {
					continue
				}
			} else {
				err = <-p.client.OutRate.Use(len(buffer))
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
	m := make(map[int]*piece)
	p.lock.RLock()
	maps.Copy(m, p.pieces)
	p.lock.RUnlock()
	now := time.Now()
	for k, v := range m {
		v.lock.RLock()
		remove := v.timeout.Before(now)
		v.lock.RUnlock()
		if remove {
			p.lock.Lock()
			delete(p.pieces, k)
			p.bail = true
			p.lock.Unlock()
			p.client.tracker.clearDownload(k)
		}
	}
}
