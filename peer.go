package torrent

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/xio"
	"github.com/richardwilkes/torrent/container/bits"
	"github.com/richardwilkes/torrent/container/spanlist"
	"github.com/richardwilkes/torrent/tio"
)

const (
	msgReadDeadline         = 5 * time.Second
	msgWriteDeadline        = 5 * time.Second
	keepAlivePeriod         = 2 * time.Minute
	downloadReadDeadline    = 10 * time.Second
	maxWaitForChunkDownload = 20 * time.Second
	chunkSize               = 16384
)

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
	has         *bits.Bits
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

func newPieceRequest(buffer []byte, cancel bool) *pieceRequest {
	return &pieceRequest{
		index:  int(binary.BigEndian.Uint32(buffer[1:5])),
		begin:  int(binary.BigEndian.Uint32(buffer[5:9])),
		length: int(binary.BigEndian.Uint32(buffer[9:])),
		cancel: cancel,
	}
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
		has:         bits.New(client.torrentFile.PieceCount()),
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
	lastReceived   time.Time
	bytesRead      int64
	bytesWritten   int64
	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool
	downloading    bool
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
		if err := tio.ReadWithDeadline(p.conn, lengthBuffer, msgReadDeadline); err != nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(p.logger, err)
			}
			p.client.dispatcher.GateKeeper().BlockAddress(p.conn.RemoteAddr())
			return
		}
		length := binary.BigEndian.Uint32(lengthBuffer)
		if length > 0 { // Not a keep-alive message
			buffer := make([]byte, length)
			if err := tio.ReadWithDeadline(p.conn, buffer, msgReadDeadline); err != nil {
				if tio.ShouldLogIOError(err) {
					errs.LogTo(p.logger, err)
				}
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
				p.lock.RLock()
				canRequest := !p.amChoking
				p.lock.RUnlock()
				if canRequest {
					p.requestChan <- newPieceRequest(buffer, false)
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
	var has *bits.Bits
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
		p.pieces[index] = &piece{
			buffer:  make([]byte, length),
			timeout: time.Now().Add(downloadReadDeadline),
		}
	}
	p.lock.Unlock()
	if !ok {
		for i := 0; i < length; i += chunkSize {
			buffer := make([]byte, 17)
			binary.BigEndian.PutUint32(buffer[:4], 13)
			buffer[4] = requestID
			binary.BigEndian.PutUint32(buffer[5:9], uint32(index))
			binary.BigEndian.PutUint32(buffer[9:13], uint32(i))
			size := chunkSize
			if length-i < chunkSize {
				size = length - i
			}
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
			n, err := p.client.file.WriteAt(one.buffer, p.client.torrentFile.OffsetOf(index))
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

func (p *peer) pieceRequestQueue() {
	queueChan := make(chan *pieceRequest)
	go p.processPieceRequests(queueChan)
	defer close(queueChan)
	queue := make(map[int]*pieceRequest)
	var head, tail int
	for {
		if head == tail {
			req, ok := <-p.requestChan
			if !ok {
				return
			}
			if !req.cancel {
				queue[head] = req
				head++
			}
		} else {
			next := queue[tail]
			if next == nil {
				tail++
			} else {
				select {
				case req, ok := <-p.requestChan:
					if !ok {
						for head != tail {
							next = queue[tail]
							if next != nil {
								queueChan <- next
								delete(queue, tail)
							}
							tail++
						}
						return
					}
					if req.cancel {
						for k, one := range queue {
							if req.index == one.index && req.begin == one.begin && req.length == one.length {
								delete(queue, k)
								break
							}
						}
					} else {
						queue[head] = req
						head++
					}
				case queueChan <- next:
					delete(queue, tail)
					tail++
				}
			}
		}
	}
}

func (p *peer) processPieceRequests(in chan *pieceRequest) {
	process := true
	for req := range in {
		if !process {
			continue
		}
		buffer := make([]byte, 13+req.length)
		binary.BigEndian.PutUint32(buffer[:4], uint32(9+req.length))
		buffer[4] = pieceID
		binary.BigEndian.PutUint32(buffer[5:9], uint32(req.index))
		binary.BigEndian.PutUint32(buffer[9:13], uint32(req.begin))
		if _, err := p.client.file.ReadAt(buffer[13:], p.client.torrentFile.OffsetOf(req.index)+int64(req.begin)); err != nil {
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
	for k, v := range p.pieces {
		m[k] = v
	}
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
