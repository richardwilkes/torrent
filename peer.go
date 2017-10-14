package torrent

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/richardwilkes/errs"
	"github.com/richardwilkes/logadapter"
)

const (
	msgReadDeadline         = 10 * time.Second
	msgWriteDeadline        = 10 * time.Second
	keepAlivePeriod         = 2 * time.Minute
	downloadReadDeadline    = 10 * time.Second
	maxWaitForChunkDownload = 15 * time.Second
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
	client         *Client
	logger         logadapter.Logger
	conn           net.Conn
	created        time.Time
	has            *bits
	requestChan    chan *pieceRequest
	writeQueue     chan []byte
	lock           sync.RWMutex
	pieces         map[int]*piece
	bytesRead      int64
	bytesWritten   int64
	lastReceived   time.Time
	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool
	bail           bool
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
	lock    sync.RWMutex
	ranges  []span
	buffer  []byte
	timeout time.Time
}

func newPeer(client *Client, conn net.Conn, log logadapter.Logger) *peer {
	return &peer{
		client:      client,
		logger:      log,
		conn:        conn,
		created:     time.Now(),
		has:         newBits(client.torrentFile.PieceCount()),
		requestChan: make(chan *pieceRequest),
		writeQueue:  make(chan []byte, 32),
		amChoking:   true,
		peerChoking: true,
		pieces:      make(map[int]*piece),
	}
}

type peerState struct {
	bytesRead      int64
	bytesWritten   int64
	lastReceived   time.Time
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
	ps := peerState{
		bytesRead:      p.bytesRead,
		bytesWritten:   p.bytesWritten,
		lastReceived:   p.lastReceived,
		amChoking:      p.amChoking,
		amInterested:   p.amInterested,
		peerChoking:    p.peerChoking,
		peerInterested: p.peerInterested,
		downloading:    len(p.pieces) > 0,
	}
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
	send := p.amChoking != choked
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
			list := make([]int, 0)
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
				// Notify other peers on the client to check for potential
				// downloads, since we may have freed up some pieces to
				// download
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
		if err := readWithDeadline(p.conn, lengthBuffer, msgReadDeadline); err != nil {
			if shouldLogIOError(err) {
				p.logger.Warn(err)
			}
			return
		}
		length := binary.BigEndian.Uint32(lengthBuffer)
		if length > 0 { // Not a keep-alive message
			buffer := make([]byte, length)
			if err := readWithDeadline(p.conn, buffer, msgReadDeadline); err != nil {
				if shouldLogIOError(err) {
					p.logger.Warn(err)
				}
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
				if length != uint32(1+len(p.has.data)) {
					p.lock.Unlock()
					p.logger.Warnf("Expected bit field to be %d bytes long, but was %d bytes long", len(p.has.data), length-1)
					return
				}
				p.has.data = buffer[1:]
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
					if shouldLogIOError(err) {
						p.logger.Warn(err)
					}
					return
				}
			case cancelID:
				p.requestChan <- newPieceRequest(buffer, true)
			case portID:
				// Ignore. DHT not implemented.
			default:
				p.logger.Warnf("Unknown ID %d", int(buffer[0]))
			}
		}
		p.lock.Lock()
		p.bytesRead += int64(length + 4)
		p.lock.Unlock()
		if err := <-p.client.InRate.Use(int(length + 4)); err != nil {
			if shouldLogIOError(err) {
				p.logger.Warn(err)
			}
			return
		}
	}
}

func (p *peer) startDownloadIfNeeded() {
	var has *bits
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
	length := p.client.torrentFile.LengthOf(index)
	p.lock.Lock()
	_, ok := p.pieces[index]
	if !ok {
		p.pieces[index] = &piece{
			ranges:  make([]span, 0),
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
		return errs.New("Received chunk for piece not requested")
	}
	last := begin + len(buffer)
	if last > len(one.buffer) {
		p.client.dispatcher.rejectAddress(p.conn.RemoteAddr())
		return errs.New("Chunk data would overrun buffer")
	}
	one.lock.Lock()
	now := time.Now()
	bailIfNotFinish := one.timeout.Before(now)
	one.timeout = now.Add(downloadReadDeadline)
	copy(one.buffer[begin:last], buffer)
	// Replacing the commented block below with this line on the theory that
	// overlaps really only cause a delay if the overlapped data is wrong, but
	// the validation on a finished block will will catch it.
	one.ranges, _ = span{start: begin, length: len(buffer)}.insertInto(one.ranges)
	// var hadOverlap bool
	// one.ranges, hadOverlap = span{start: begin, length: len(buffer)}.insertInto(one.ranges)
	// if hadOverlap {
	// 	// Shouldn't happen... don't trust this peer
	// 	one.lock.Unlock()
	// 	p.client.dispatcher.rejectAddress(p.conn.RemoteAddr())
	// 	return errs.New("Chunk data would overlap existing data -- don't trust this peer")
	// }
	p.lock.Lock()
	p.lastReceived = now
	p.lock.Unlock()
	if len(one.ranges) == 1 && one.ranges[0].start == 0 && one.ranges[0].length == len(one.buffer) {
		if p.client.torrentFile.Validate(index, one.buffer) {
			n, err := p.client.file.WriteAt(one.buffer, p.client.torrentFile.OffsetOf(index))
			one.lock.Unlock()
			p.lock.Lock()
			delete(p.pieces, index)
			p.lock.Unlock()
			if err != nil && (err != io.EOF || n != len(one.buffer)) {
				p.client.tracker.clearDownload(index)
				p.logger.Error(errs.NewfWithCause(err, "Unable to write piece %d", index))
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
			p.client.dispatcher.rejectAddress(p.conn.RemoteAddr())
			return errs.Newf("Received invalid piece %d", index)
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
	for req := range in {
		buffer := make([]byte, 13+req.length)
		binary.BigEndian.PutUint32(buffer[:4], uint32(9+req.length))
		buffer[4] = pieceID
		binary.BigEndian.PutUint32(buffer[5:9], uint32(req.index))
		binary.BigEndian.PutUint32(buffer[9:13], uint32(req.begin))
		if _, err := p.client.file.ReadAt(buffer[13:], p.client.torrentFile.OffsetOf(req.index)+int64(req.begin)); err != nil {
			p.logger.Error(errs.NewfWithCause(err, "Unable to read piece %d (begin=%d, length=%d)", req.index, req.begin, req.length))
			p.client.disconnect(p.conn)
			return
		}
		p.writeQueue <- buffer
	}
}

func (p *peer) processWriteQueue() {
	var lastWriteTime time.Time
	done := make(chan bool)
	go p.keepAlive(done)
	defer close(done)
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
				err = writeWithDeadline(p.conn, buffer, msgWriteDeadline)
			}
		}
		if err != nil || buffer == nil {
			if shouldLogIOError(err) {
				p.logger.Warn(err)
			}
			p.client.disconnect(p.conn)
			// Drain any remaining entries in the queue
			for {
				select {
				case <-p.writeQueue:
				default:
					return
				}
			}
		}
		p.lock.Lock()
		p.bytesWritten += int64(len(buffer))
		p.lock.Unlock()
	}
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
