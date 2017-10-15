package torrent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/richardwilkes/errs"
	"github.com/richardwilkes/fileutil"
	"github.com/richardwilkes/logadapter"
	"github.com/richardwilkes/rate"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

const (
	version                    = "-RW0001-"
	desiredConcurrentDownloads = 4
)

var (
	errStopRequested = errors.New("Stop requested")
)

// Client provides the ability to download and/or seed a torrent.
type Client struct {
	InRate                   rate.Limiter
	OutRate                  rate.Limiter
	dispatcher               *dispatcher.Dispatcher
	torrentFile              *tfs.File
	downloadCompleteNotifier chan *Client
	stoppedNotifier          chan *Client
	logger                   *logadapter.Prefixer
	id                       [dispatcher.PeerIDSize]byte
	tracker                  *tracker
	peersWanted              int
	peerWaitGroup            *sync.WaitGroup
	peerMgmtStop             chan bool
	file                     *os.File
	seedDuration             time.Duration
	lock                     sync.RWMutex
	peers                    map[net.Conn]*peer
	stoppedChan              chan bool
	stopRequested            bool
	stopped                  bool
}

// DownloadCap sets the maximum download speed of the client, subject to the
// dispatcher's overall limit. Default is no limit.
func DownloadCap(bytesPerSecond int) func(*Client) error {
	return func(c *Client) error {
		if bytesPerSecond < 1 {
			return errs.New("DownloadCap must be at least 1")
		}
		c.InRate.SetCap(bytesPerSecond)
		return nil
	}
}

// UploadCap sets the maximum upload speed of the client, subject to the
// dispatcher's overall limit. Default is no limit.
func UploadCap(bytesPerSecond int) func(*Client) error {
	return func(c *Client) error {
		if bytesPerSecond < 1 {
			return errs.New("UploadCap must be at least 1")
		}
		c.OutRate.SetCap(bytesPerSecond)
		return nil
	}
}

// PeersWanted sets the number of peers to ask the tracker for. Default is 30.
func PeersWanted(wanted int) func(*Client) error {
	return func(c *Client) error {
		if wanted < 1 {
			return errs.New("PeersWanted must be at least 1")
		}
		c.peersWanted = wanted
		return nil
	}
}

// SeedDuration sets the maximum amount of time to seed. Default is 4 days.
func SeedDuration(duration time.Duration) func(*Client) error {
	return func(c *Client) error {
		if duration < 0 {
			return errs.New("duration must be at least 0")
		}
		c.seedDuration = duration
		return nil
	}
}

// NotifyWhenDownloadComplete sets a channel to notify when the download is
// verified and completes.
func NotifyWhenDownloadComplete(notifier chan *Client) func(*Client) error {
	return func(c *Client) error {
		c.downloadCompleteNotifier = notifier
		return nil
	}
}

// NotifyWhenStopped sets a channel to notify when the client is stopped.
func NotifyWhenStopped(notifier chan *Client) func(*Client) error {
	return func(c *Client) error {
		c.stoppedNotifier = notifier
		return nil
	}
}

// NewClient creates and starts a new client for a torrent.
func NewClient(d *dispatcher.Dispatcher, torrentFile *tfs.File, options ...func(*Client) error) (*Client, error) {
	if d == nil {
		return nil, errs.New("dispatcher may not be nil")
	}
	if torrentFile == nil {
		return nil, errs.New("torrentFile may not be nil")
	}
	_, prefix := filepath.Split(torrentFile.StoragePath())
	prefix = prefix[:len(prefix)-len(filepath.Ext(prefix))] + " | "
	c := &Client{
		InRate:        d.InRate.New(math.MaxInt32),
		OutRate:       d.OutRate.New(math.MaxInt32),
		dispatcher:    d,
		torrentFile:   torrentFile,
		logger:        &logadapter.Prefixer{Logger: d.Logger(), Prefix: prefix},
		peersWanted:   16,
		peerWaitGroup: &sync.WaitGroup{},
		peerMgmtStop:  make(chan bool),
		seedDuration:  96 * time.Hour,
		peers:         make(map[net.Conn]*peer),
		stoppedChan:   make(chan bool, 1),
	}
	const urlQuerySafeBytes = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_.~"
	for i := range version {
		c.id[i] = version[i]
	}
	rand.Read(c.id[len(version):])
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

// Logger returns the client's logger.
func (c *Client) Logger() logadapter.Logger {
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
	var err error
	if c.file, err = os.OpenFile(c.torrentFile.StoragePath(), os.O_CREATE|os.O_RDWR, 0644); err != nil {
		c.finish(errs.Wrap(err))
		return
	}
	if err = c.prepareFile(); err != nil {
		c.finish(err)
		return
	}
	if c.shouldStop() {
		c.finish(errStopRequested)
		return
	}
	c.dispatcher.Register(c.torrentFile.InfoHash, c)
	defer c.dispatcher.Deregister(c.torrentFile.InfoHash)
	if err = c.tracker.announceStart(); err != nil {
		c.finish(err)
		return
	}
	if !c.tracker.isDownloadComplete() {
		c.tracker.setStateAndProgress(Downloading, -1)
	} else {
		c.tracker.setState(Seeding)
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
	fi, err := c.file.Stat()
	if err != nil {
		return errs.Wrap(err)
	}
	if fi.IsDir() {
		return errs.Newf("%s may not be a directory", c.torrentFile.StoragePath())
	}
	length := c.torrentFile.Size()
	original := fi.Size()
	if original == 0 {
		if err = c.file.Truncate(length); err != nil {
			return errs.Wrap(err)
		}
	} else {
		if original != length {
			if err = c.file.Truncate(length); err != nil {
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
			if n, err = c.file.ReadAt(buffer, pos); err != nil && err != io.EOF {
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
	return nil
}

// HandleConnection is called by the dispatcher for new connections.
func (c *Client) HandleConnection(conn net.Conn, log logadapter.Logger, extensions [dispatcher.ExtensionsSize]byte, infoHash [sha1.Size]byte, sendHandshake bool) {
	log = &logadapter.Prefixer{Logger: log, Prefix: c.logger.Prefix}
	if !bytes.Equal(infoHash[:], c.torrentFile.InfoHash[:]) {
		log.Warn("Rejecting due to InfoHash mis-match")
		return
	}
	if sendHandshake {
		var myExtensions [dispatcher.ExtensionsSize]byte
		if err := dispatcher.SendTorrentHandshake(conn, myExtensions, c.torrentFile.InfoHash, c.id); err != nil {
			if tio.ShouldLogIOError(err) {
				log.Warn(err)
			}
			return
		}
	}
	var peerID [dispatcher.PeerIDSize]byte
	if err := tio.ReadWithDeadline(conn, peerID[:], dispatcher.HandshakeDeadline); err != nil {
		if tio.ShouldLogIOError(err) {
			log.Warn(err)
		}
		return
	}
	p := newPeer(c, conn, log)
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
	c.peerWaitGroup.Add(1)
	defer c.peerWaitGroup.Done()
	p.processIncomingMessages()
	c.disconnect(conn)
}

func (c *Client) disconnect(conn net.Conn) {
	fileutil.CloseIgnoringErrors(conn)
	c.lock.Lock()
	delete(c.peers, conn)
	c.lock.Unlock()
}

func (c *Client) connectToPeer(addr string, port int) {
	raddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", addr, port))
	if err != nil {
		if tio.ShouldLogIOError(err) {
			c.logger.Warn(errs.Wrap(err))
		}
		return
	}
	conn, err := net.DialTCP("tcp", nil, raddr)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			c.logger.Warn(errs.Wrap(err))
		}
		return
	}
	log := &logadapter.Prefixer{Logger: c.dispatcher.Logger(), Prefix: conn.RemoteAddr().String() + " | "}
	defer fileutil.CloseIgnoringErrors(conn)
	var myExtensions [dispatcher.ExtensionsSize]byte
	if err = dispatcher.SendTorrentHandshake(conn, myExtensions, c.torrentFile.InfoHash, c.id); err != nil {
		if tio.ShouldLogIOError(err) {
			log.Warn(err)
		}
		return
	}
	extensions, infoHash, err := dispatcher.ReceiveTorrentHandshake(conn)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			log.Warn(err)
		}
		return
	}
	c.HandleConnection(conn, log, extensions, infoHash, false)
}

func (c *Client) finish(err error) {
	c.tracker.setState(Stopping)
	var state State
	if err != nil && err != errStopRequested {
		state = Errored
	} else {
		state = Done
	}
	if err != nil {
		if err == errStopRequested {
			c.logger.Info(err)
		} else if tio.ShouldLogIOError(err) {
			c.logger.Warn(err)
		}
	}
	close(c.peerMgmtStop)
	c.lock.Lock()
	for conn := range c.peers {
		fileutil.CloseIgnoringErrors(conn)
	}
	c.lock.Unlock()
	c.peerWaitGroup.Wait()
	if err = c.tracker.announceStopped(); err != nil {
		c.logger.Warn(err)
	}
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			c.logger.Warn(errs.Wrap(err))
		}
		c.file = nil
	}
	c.tracker.setState(state)
}

func (c *Client) managePeers() {
	c.adjustPeers()
	for {
		select {
		case <-time.After(time.Minute):
			for _, p := range c.currentPeers() {
				p.clearExpiredDownloads()
			}
		case <-time.After(10 * time.Second):
			c.adjustPeers()
		case <-c.peerMgmtStop:
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
				c.logger.Infof("AdjustPeers: Disconnecting peer due to lack of progress: %s", data.peer.conn.RemoteAddr().String())
				c.dispatcher.GateKeeper().BlockAddress(data.peer.conn.RemoteAddr())
				c.disconnect(data.peer.conn)
				continue
			}
			if !data.state.peerChoking && now.Sub(data.state.lastReceived) <= maxWaitForChunkDownload {
				downloadCount++
			}
		}
		pd = append(pd, data)
	}
	if downloadCount < desiredConcurrentDownloads && !c.tracker.isSeedingComplete() {
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
			c.logger.Infof("AdjustPeers: Disconnecting peer to make room: %s", pd[0].peer.conn.RemoteAddr().String())
			c.disconnect(pd[0].peer.conn)
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
						c.logger.Infof("AdjustPeers: Attempting to connect to new peer: %s", addr)
						go c.connectToPeer(addr, port)
						existing[addr] = true
						added = true
						break
					}
				}
				if !added {
					c.logger.Warnf("Unable to add new peer due to lack of unconnected peers (checked %d)", len(peerAddressMap))
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
	c.logger.Infof("dropPeerIfPossible: Disconnecting peer to make room: %s", pd[0].peer.conn.RemoteAddr().String())
	c.disconnect(pd[0].peer.conn)
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
