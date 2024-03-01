package torrent

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/rate"
	"github.com/richardwilkes/toolbox/xio"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

const (
	version           = "-RW0001-"
	urlQuerySafeBytes = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_.~"
)

var errStopRequested = errors.New("stop requested")

// Client provides the ability to download and/or seed a torrent.
type Client struct {
	InRate                   rate.Limiter
	OutRate                  rate.Limiter
	dispatcher               *dispatcher.Dispatcher
	torrentFile              *tfs.File
	downloadCompleteNotifier chan *Client
	stoppedNotifier          chan *Client
	logger                   *slog.Logger
	id                       dispatcher.PeerID
	_                        int32 // for 8-byte alignment, since id is 20 bytes
	tracker                  *tracker
	concurrentDownloads      int
	peersWanted              int
	peerWaitGroup            *sync.WaitGroup
	peerMgmtLock             sync.Mutex
	peerMgmtStop             chan chan bool
	file                     *os.File
	seedDuration             time.Duration
	lock                     sync.RWMutex
	peers                    map[net.Conn]*peer
	stoppedChan              chan bool
	stopRequested            bool
	stopped                  bool
}

// NewClient creates and starts a new client for a torrent.
func NewClient(d *dispatcher.Dispatcher, torrentFile *tfs.File, options ...func(*Client) error) (*Client, error) {
	if d == nil {
		return nil, errs.New("dispatcher may not be nil")
	}
	if torrentFile == nil {
		return nil, errs.New("torrentFile may not be nil")
	}
	_, storagePath := filepath.Split(torrentFile.StoragePath())
	c := &Client{
		InRate:              d.InRate.New(math.MaxInt32),
		OutRate:             d.OutRate.New(math.MaxInt32),
		dispatcher:          d,
		torrentFile:         torrentFile,
		logger:              d.Logger().With("torrent_file", storagePath[:len(storagePath)-len(filepath.Ext(storagePath))]),
		concurrentDownloads: 4,
		peersWanted:         32,
		peerWaitGroup:       &sync.WaitGroup{},
		peerMgmtStop:        make(chan chan bool, 1),
		seedDuration:        96 * time.Hour,
		peers:               make(map[net.Conn]*peer),
		stoppedChan:         make(chan bool, 1),
	}
	copy(c.id[:], version)
	if _, err := rand.Read(c.id[len(version):]); err != nil {
		return nil, errs.Wrap(err)
	}
	for i := len(version); i < len(c.id); i++ {
		c.id[i] = urlQuerySafeBytes[int(c.id[i])%len(urlQuerySafeBytes)]
	}
	for _, option := range options {
		if err := option(c); err != nil {
			return nil, err
		}
	}
	c.tracker = newTracker(c)
	go c.run()
	return c, nil
}

// ExternalIP returns our external IP address.
func (c *Client) ExternalIP() string {
	return c.dispatcher.ExternalIP()
}

// Logger returns the client's logger.
func (c *Client) Logger() *slog.Logger {
	return c.logger
}

// TorrentFile returns the client's torrent file.
func (c *Client) TorrentFile() *tfs.File {
	return c.torrentFile
}

// Stop the torrent. Does not return until the torrent has stopped or the
// timeout has been hit.
func (c *Client) Stop(timeout time.Duration) {
	c.lock.Lock()
	if c.stopped {
		c.lock.Unlock()
		return
	}
	c.stopRequested = true
	c.lock.Unlock()
	c.closeAllPeers()
	select {
	case <-time.After(timeout):
		if c.stoppedNotifier != nil {
			// Notify that we've stopped, but only if we won't block
			select {
			case c.stoppedNotifier <- c:
			default:
			}
		}
	case <-c.stoppedChan:
	}
}

func (c *Client) shouldStop() bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.stopRequested
}

// Status returns the current status of the torrent.
func (c *Client) Status() *Status {
	c.tracker.lock.RLock()
	peersDownloading := len(c.tracker.who)
	c.tracker.lock.RUnlock()
	c.lock.RLock()
	peersConnected := len(c.peers)
	c.lock.RUnlock()
	return c.tracker.status(peersDownloading, peersConnected)
}

func (c *Client) run() {
	defer func() {
		close(c.stoppedChan)
		if c.stoppedNotifier != nil {
			c.stoppedNotifier <- c
		}
	}()
	if err := c.prepareFile(); err != nil {
		c.finish(err)
		return
	}
	c.dispatcher.Register(c.torrentFile.InfoHash, c)
	defer c.dispatcher.Deregister(c.torrentFile.InfoHash)
	if err := c.tracker.announceStart(); err != nil {
		c.finish(err)
		return
	}
	go c.managePeers()
	for {
		if c.tracker.isSeedingComplete() {
			c.finish(nil)
			return
		}
		if c.shouldStop() {
			c.finish(errStopRequested)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *Client) prepareFile() error {
	f, err := os.OpenFile(c.torrentFile.StoragePath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return errs.Wrap(err)
	}
	c.file = f
	fi, err := f.Stat()
	if err != nil {
		return errs.Wrap(err)
	}
	if fi.IsDir() {
		return errs.Newf("%s may not be a directory", c.torrentFile.StoragePath())
	}
	length := c.torrentFile.Size()
	original := fi.Size()
	if original == 0 {
		if err = f.Truncate(length); err != nil {
			return errs.Wrap(err)
		}
	} else {
		if original != length {
			if err = f.Truncate(length); err != nil {
				return errs.Wrap(err)
			}
			if c.shouldStop() {
				return errStopRequested
			}
		}
		buffer := make([]byte, c.torrentFile.Info.PieceLength)
		count := c.torrentFile.PieceCount()
		lastPieceLength := int(length - int64(count-1)*int64(c.torrentFile.Info.PieceLength))
		for i := 0; i < count; i++ {
			pos := int64(i) * int64(c.torrentFile.Info.PieceLength)
			if pos >= original {
				break
			}
			var n int
			if n, err = f.ReadAt(buffer, pos); err != nil && !errors.Is(err, io.EOF) {
				return errs.Wrap(err)
			}
			if n != c.torrentFile.Info.PieceLength {
				if i != count-1 || n != lastPieceLength {
					return errs.New("Unable to read file: " + c.torrentFile.StoragePath())
				}
			}
			if c.torrentFile.Validate(i, buffer[:n]) {
				c.tracker.markBlockValid(i)
			}
			c.tracker.setProgress(float64(i+1) * 100 / float64(count))
			if c.shouldStop() {
				return errStopRequested
			}
		}
	}
	c.tracker.setProgress(100)
	if c.shouldStop() {
		return errStopRequested
	}
	return nil
}

// HandleConnection is called by the dispatcher for new connections.
func (c *Client) HandleConnection(conn net.Conn, logger *slog.Logger, _ dispatcher.ProtocolExtensions, infoHash tfs.InfoHash, sendHandshake bool) {
	_, storagePath := filepath.Split(c.torrentFile.StoragePath())
	logger = logger.With("torrent_file", storagePath[:len(storagePath)-len(filepath.Ext(storagePath))])
	if !bytes.Equal(infoHash[:], c.torrentFile.InfoHash[:]) {
		c.dispatcher.GateKeeper().BlockAddress(conn.RemoteAddr())
		return
	}
	if sendHandshake {
		var myExtensions dispatcher.ProtocolExtensions
		if err := dispatcher.SendTorrentHandshake(conn, myExtensions, c.torrentFile.InfoHash, c.id); err != nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(logger, err)
			}
			c.dispatcher.GateKeeper().BlockAddress(conn.RemoteAddr())
			return
		}
	}
	var peerID dispatcher.PeerID
	if err := tio.ReadWithDeadline(conn, peerID[:], dispatcher.HandshakeDeadline); err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		c.dispatcher.GateKeeper().BlockAddress(conn.RemoteAddr())
		return
	}
	p := newPeer(c, conn, logger)
	if c.shouldStop() {
		return
	}
	c.lock.RLock()
	needRoom := len(c.peers) >= c.peersWanted
	c.lock.RUnlock()
	if needRoom {
		if !c.dropPeerIfPossible() {
			return
		}
	}
	c.lock.Lock()
	c.peers[conn] = p
	c.lock.Unlock()
	c.peerMgmtLock.Lock()
	if c.peerMgmtStop == nil {
		c.peerMgmtLock.Unlock()
		return
	}
	c.peerWaitGroup.Add(1)
	c.peerMgmtLock.Unlock()
	defer func() {
		xio.CloseIgnoringErrors(conn)
		c.lock.Lock()
		delete(c.peers, conn)
		c.lock.Unlock()
		c.peerWaitGroup.Done()
	}()
	p.processIncomingMessages()
}

func (c *Client) connectToPeer(addr string, port int) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", addr, port), 5*time.Second)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(c.logger, err)
		}
		c.dispatcher.GateKeeper().BlockAddressString(addr)
		return
	}
	defer xio.CloseIgnoringErrors(conn)
	logger := c.dispatcher.Logger().With("remote_addr", conn.RemoteAddr().String())
	var myExtensions dispatcher.ProtocolExtensions
	if err = dispatcher.SendTorrentHandshake(conn, myExtensions, c.torrentFile.InfoHash, c.id); err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		c.dispatcher.GateKeeper().BlockAddressString(addr)
		return
	}
	var extensions dispatcher.ProtocolExtensions
	var infoHash tfs.InfoHash
	if extensions, infoHash, err = dispatcher.ReceiveTorrentHandshake(conn); err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		c.dispatcher.GateKeeper().BlockAddressString(addr)
		return
	}
	c.HandleConnection(conn, logger, extensions, infoHash, false)
}

func (c *Client) finish(err error) {
	c.tracker.setState(Stopping)
	var state State
	if err != nil && !errors.Is(err, errStopRequested) {
		state = Errored
	} else {
		state = Done
	}
	if err != nil {
		if errors.Is(err, errStopRequested) {
			c.logger.Info("stop requested")
		} else if tio.ShouldLogIOError(err) {
			errs.LogTo(c.logger, err)
		}
	}
	c.closeAllPeers()
	c.peerWaitGroup.Wait()
	if err = c.tracker.announceStopped(); err != nil {
		errs.LogTo(c.logger, err)
	}
	if c.file != nil {
		if err = c.file.Close(); err != nil {
			errs.LogTo(c.logger, err)
		}
		c.file = nil
	}
	c.tracker.setState(state)
}

func (c *Client) closeAllPeers() {
	var ch chan bool
	c.peerMgmtLock.Lock()
	if c.peerMgmtStop != nil {
		ch = make(chan bool)
		c.peerMgmtStop <- ch
		c.peerMgmtStop = nil
	}
	c.peerMgmtLock.Unlock()
	if ch != nil {
		select {
		case <-time.After(time.Minute):
		case <-ch:
		}
	}
	for _, p := range c.currentPeers() {
		xio.CloseIgnoringErrors(p.conn)
	}
}

func (c *Client) managePeers() {
	c.peerMgmtLock.Lock()
	stopChan := c.peerMgmtStop
	c.peerMgmtLock.Unlock()
	c.adjustPeers()
	for {
		select {
		case <-time.After(time.Minute):
			for _, p := range c.currentPeers() {
				p.clearExpiredDownloads()
			}
		case <-time.After(10 * time.Second):
			c.adjustPeers()
		case ch := <-stopChan:
			ch <- true
			return
		}
	}
}

type peerData struct {
	peer  *peer
	state peerState
}

func (c *Client) adjustPeers() {
	peers := c.currentPeers()
	pd := make([]*peerData, 0, len(peers))
	now := time.Now()
	downloadCount := 0
	for _, p := range peers {
		data := &peerData{
			peer:  p,
			state: p.updateInterest(),
		}
		if data.state.downloading {
			if data.state.peerChoking || now.Sub(data.state.lastReceived) > maxWaitForChunkDownload {
				c.dispatcher.GateKeeper().BlockAddress(data.peer.conn.RemoteAddr())
				xio.CloseIgnoringErrors(data.peer.conn)
				continue
			}
			if !data.state.peerChoking && now.Sub(data.state.lastReceived) <= maxWaitForChunkDownload {
				downloadCount++
			}
		}
		pd = append(pd, data)
	}
	if downloadCount < c.concurrentDownloads && !c.tracker.isSeedingComplete() {
		existing := make(map[string]bool)
		for _, one := range pd {
			if host, _, err := net.SplitHostPort(one.peer.conn.RemoteAddr().String()); err != nil {
				existing[host] = true
			}
		}
		count := c.peersWanted - len(pd)
		if count < 1 && len(pd) > 0 {
			// Find one to disconnect so we can add an alternate
			sort.Slice(pd, func(i, j int) bool {
				if !pd[i].state.amInterested && pd[j].state.amInterested {
					return true
				}
				if !pd[i].state.downloading && pd[j].state.downloading {
					return true
				}
				if pd[i].state.peerChoking && !pd[j].state.peerChoking {
					return true
				}
				if now.Sub(pd[i].state.lastReceived) > maxWaitForChunkDownload && now.Sub(pd[j].state.lastReceived) <= maxWaitForChunkDownload {
					return true
				}
				if pd[i].peer.bytesRead < pd[j].peer.bytesRead {
					return true
				}
				return pd[i].peer.created.After(pd[j].peer.created)
			})
			xio.CloseIgnoringErrors(pd[0].peer.conn)
			pd = pd[1:]
			count = 1
		}
		if count > 0 {
			if count > 4 {
				count = 4
			}
			peerAddressMap := c.tracker.peerAddressesMap()
			for i := 0; i < count; i++ {
				added := false
				for addr, port := range peerAddressMap {
					if _, exists := existing[addr]; !exists && !c.dispatcher.GateKeeper().IsAddressStringBlocked(addr) {
						go c.connectToPeer(addr, port)
						existing[addr] = true
						added = true
						break
					}
				}
				if !added {
					break
				}
			}
		}
	}
	sort.Slice(pd, func(i, j int) bool {
		if pd[i].state.amInterested && !pd[j].state.amInterested {
			return true
		}
		if pd[i].state.downloading && !pd[j].state.downloading {
			return true
		}
		if !pd[i].state.peerChoking && pd[j].state.peerChoking {
			return true
		}
		if pd[i].state.peerInterested && !pd[j].state.peerInterested {
			return true
		}
		if now.Sub(pd[i].state.lastReceived) <= maxWaitForChunkDownload && now.Sub(pd[j].state.lastReceived) > maxWaitForChunkDownload {
			return true
		}
		if pd[i].state.bytesRead > pd[j].state.bytesRead {
			return true
		}
		if pd[i].state.bytesWritten > pd[j].state.bytesWritten {
			return true
		}
		return pd[i].peer.created.After(pd[j].peer.created)
	})
	for i, one := range pd {
		one.peer.setChoked(i > 3 && !one.state.downloading)
	}
}

func (c *Client) dropPeerIfPossible() bool {
	peers := c.currentPeers()
	pd := make([]peerData, 0, len(peers))
	for _, p := range peers {
		state := p.updateInterest()
		if !state.downloading && !state.amInterested {
			pd = append(pd, peerData{peer: p, state: state})
		}
	}
	switch len(pd) {
	case 0:
		return false
	case 1:
	default:
		sort.Slice(pd, func(i, j int) bool {
			if pd[i].state.peerChoking && !pd[j].state.peerChoking {
				return true
			}
			if pd[i].peer.bytesRead < pd[j].peer.bytesRead {
				return true
			}
			return pd[i].peer.created.After(pd[j].peer.created)
		})
	}
	xio.CloseIgnoringErrors(pd[0].peer.conn)
	return true
}

func (c *Client) currentPeers() []*peer {
	c.lock.RLock()
	peers := make([]*peer, 0, len(c.peers))
	for _, p := range c.peers {
		peers = append(peers, p)
	}
	c.lock.RUnlock()
	return peers
}

func (c *Client) informPeersWeHavePiece(index int) {
	buffer := make([]byte, 9)
	binary.BigEndian.PutUint32(buffer[:4], 5)
	buffer[4] = haveID
	binary.BigEndian.PutUint32(buffer[5:], uint32(index))
	for _, one := range c.currentPeers() {
		one.writeQueue <- buffer
	}
}
