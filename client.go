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
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/rate"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

const (
	peerDialTimeout                   = 5 * time.Second
	peerAdjustmentInterval            = 15 * time.Second
	peerClearExpiredDownloadsInterval = time.Minute
	// stoppedNotifyTimeout is how long we'll hold onto the stopped notification waiting for it to be accepted. A
	// buffered channel is expected, so this only comes into play for a consumer that has stopped listening.
	stoppedNotifyTimeout = time.Minute
	// downloadCompleteNotifyTimeout is how long the goroutine delivering the download complete notification will wait
	// for it to be accepted before giving up, for a consumer that isn't listening when it arrives.
	downloadCompleteNotifyTimeout = time.Minute
)

const urlQuerySafeBytes = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_.~"

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
	tracker                  *tracker
	peerWaitGroup            *sync.WaitGroup
	file                     *os.File
	peerMgmtStop             chan struct{}      // closed when peer management should stop
	peerMgmtDone             chan struct{}      // protected by peerMgmtLock
	peers                    map[net.Conn]*peer // protected by lock
	dialing                  map[string]bool    // protected by lock
	stoppedChan              chan bool          // protected by lock
	concurrentDownloads      int
	peersWanted              int
	seedDuration             time.Duration
	peerMgmtLock             sync.Mutex
	lock                     sync.RWMutex
	id                       dispatcher.PeerID
	stopRequested            bool // protected by lock
	stopped                  bool // protected by lock
	stoppedNotified          bool // protected by lock
	downloadCompleteNotified bool // protected by lock
	peerMgmtStopping         bool // protected by peerMgmtLock
}

// NewClient creates and starts a new client for a torrent.
func NewClient(d *dispatcher.Dispatcher, torrentFile *tfs.File, options ...func(*Client) error) (*Client, error) {
	if d == nil {
		return nil, errs.New("dispatcher may not be nil")
	}
	if torrentFile == nil {
		return nil, errs.New("torrentFile may not be nil")
	}
	_, path := filepath.Split(torrentFile.StoragePath())
	c := &Client{
		InRate:              d.InRate.New(math.MaxInt32),
		OutRate:             d.OutRate.New(math.MaxInt32),
		dispatcher:          d,
		torrentFile:         torrentFile,
		logger:              d.Logger().With("torrent_file", path[:len(path)-len(filepath.Ext(path))]),
		concurrentDownloads: 4,
		peersWanted:         32,
		peerWaitGroup:       &sync.WaitGroup{},
		peerMgmtStop:        make(chan struct{}),
		seedDuration:        96 * time.Hour,
		peers:               make(map[net.Conn]*peer),
		dialing:             make(map[string]bool),
		stoppedChan:         make(chan bool, 1),
	}
	if _, err := rand.Read(c.id[:]); err != nil {
		c.closeRateLimiters()
		return nil, errs.Wrap(err)
	}
	for i := range c.id {
		c.id[i] = urlQuerySafeBytes[int(c.id[i])%len(urlQuerySafeBytes)]
	}
	for _, option := range options {
		if err := option(c); err != nil {
			c.closeRateLimiters()
			return nil, err
		}
	}
	c.tracker = newTracker(c)
	go c.run()
	return c, nil
}

// ExternalIP returns our external IP address.
func (c *Client) ExternalIP() net.IP {
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

// storageFile returns the file the torrent's data is stored in, or nil if it has already been closed. Peer goroutines
// that aren't tracked by the peer wait group can still be draining work while the client shuts down, so the file must
// only ever be reached through this.
func (c *Client) storageFile() *os.File {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.file
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
		// Notify that we've stopped, but only if we won't block
		c.notifyStopped(0)
	case <-c.stoppedChan:
	}
}

// notifyStopped delivers the stopped notification, if one was asked for and one hasn't been delivered already. Waits
// up to 'wait' for it to be accepted, so that a consumer that is no longer listening can't leave us blocked forever.
// A 'wait' of zero or less means the notification is only delivered if it can be done without blocking.
func (c *Client) notifyStopped(wait time.Duration) {
	if c.stoppedNotifier == nil {
		return
	}
	c.lock.Lock()
	if c.stoppedNotified {
		c.lock.Unlock()
		return
	}
	if wait <= 0 {
		// Only claim the notification if it can be handed off right now, so that a later attempt can still deliver it
		select {
		case c.stoppedNotifier <- c:
			c.stoppedNotified = true
		default:
		}
		c.lock.Unlock()
		return
	}
	c.stoppedNotified = true
	c.lock.Unlock()
	select {
	case c.stoppedNotifier <- c:
	case <-time.After(wait):
	}
}

// notifyDownloadComplete delivers the download complete notification, if one was asked for and one hasn't been
// delivered already. The callers are the client's run goroutine and the peers' read goroutines, neither of which may
// be left blocked on a consumer that isn't listening: doing so would keep the client from ever finishing its startup,
// or leave finish() waiting on a peer that can never return. Delivery therefore falls back to a goroutine with a
// bounded wait whenever it can't be completed immediately.
func (c *Client) notifyDownloadComplete() {
	if c.downloadCompleteNotifier == nil {
		return
	}
	c.lock.Lock()
	if c.downloadCompleteNotified {
		c.lock.Unlock()
		return
	}
	c.downloadCompleteNotified = true
	c.lock.Unlock()
	select {
	case c.downloadCompleteNotifier <- c:
	default:
		go func() {
			select {
			case c.downloadCompleteNotifier <- c:
			case <-time.After(downloadCompleteNotifyTimeout):
			}
		}()
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
		c.notifyStopped(stoppedNotifyTimeout)
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
	c.startPeerManagement()
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
	c.lock.Lock()
	c.file = f
	c.lock.Unlock()
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
		lastPieceLength := length - int64(count-1)*c.torrentFile.Info.PieceLength
		for i := range count {
			pos := int64(i) * c.torrentFile.Info.PieceLength
			if pos >= original {
				break
			}
			var n int
			if n, err = f.ReadAt(buffer, pos); err != nil && !errors.Is(err, io.EOF) {
				return errs.Wrap(err)
			}
			if int64(n) != c.torrentFile.Info.PieceLength {
				if i != count-1 || int64(n) != lastPieceLength {
					return errs.New("unable to read file: " + c.torrentFile.StoragePath())
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
	remoteAddr := conn.RemoteAddr()
	logger = logger.With("torrent_file", storagePath[:len(storagePath)-len(filepath.Ext(storagePath))],
		"remote_addr", remoteAddr.String())
	logger.Debug("new connection")
	if !bytes.Equal(infoHash[:], c.torrentFile.InfoHash[:]) {
		c.dispatcher.GateKeeper().BlockAddress(remoteAddr)
		return
	}
	if sendHandshake {
		var myExtensions dispatcher.ProtocolExtensions
		if err := dispatcher.SendTorrentHandshake(conn, myExtensions, c.torrentFile.InfoHash, c.id); err != nil {
			if tio.ShouldLogIOError(err) {
				errs.LogTo(logger, err)
			}
			c.dispatcher.GateKeeper().BlockAddress(remoteAddr)
			return
		}
	}
	var peerID dispatcher.PeerID
	if err := tio.ReadWithDeadline(conn, peerID[:], dispatcher.HandshakeDeadline); err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		c.dispatcher.GateKeeper().BlockAddress(remoteAddr)
		return
	}
	// A remote presenting our own ID is us. That happens when the tracker hands back our own address, which parsePeers
	// can only filter out when the external IP lookup succeeded, so both the dial we made and the connection it arrives
	// on land here. Severing it is standard practice and keeps a connection to ourselves from occupying two of our
	// limited peer slots; blocking the address as well keeps the next peer adjustment from simply dialing it again.
	if peerID == c.id {
		logger.Debug("refusing connection to ourselves")
		c.dispatcher.GateKeeper().BlockAddress(remoteAddr)
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
	// The peer is registered while holding the peer management lock so that it is either fully tracked before the stop
	// begins, and therefore closed by closeAllPeers, or not added at all. Adding it to the map first would leave a
	// stale entry behind, with a write queue nothing would ever drain, when the client is already stopping.
	c.peerMgmtLock.Lock()
	if c.peerMgmtStopping {
		c.peerMgmtLock.Unlock()
		return
	}
	c.peerWaitGroup.Add(1)
	c.lock.Lock()
	c.peers[conn] = p
	c.lock.Unlock()
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

// connectToPeer dials the peer and, if the handshake exchange succeeds, hands the connection off to be serviced. The
// host is claimed by the caller through startDial before this is started, so that nothing else can dial it in the
// window before this goroutine runs, and stays claimed for as long as we're working with the connection.
func (c *Client) connectToPeer(addr string, port int) {
	defer c.finishDial(addr)
	slog.Debug("dialing peer", "address", addr, "port", port)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(port)), peerDialTimeout)
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
	// Peer goroutines that the wait group doesn't track can still be draining work at this point, so take the file
	// away under the lock rather than clearing the field out from under them.
	c.lock.Lock()
	f := c.file
	c.file = nil
	c.lock.Unlock()
	if f != nil {
		if err = f.Close(); err != nil {
			errs.LogTo(c.logger, err)
		}
	}
	c.closeRateLimiters()
	c.tracker.setState(state)
	c.lock.Lock()
	c.stopped = true
	c.lock.Unlock()
}

// closeRateLimiters releases our rate limiters from the dispatcher's limiter tree, which would otherwise continue to
// traverse them on every tick for the life of the dispatcher.
func (c *Client) closeRateLimiters() {
	c.InRate.Close()
	c.OutRate.Close()
}

func (c *Client) closeAllPeers() {
	c.peerMgmtLock.Lock()
	if !c.peerMgmtStopping {
		c.peerMgmtStopping = true
		close(c.peerMgmtStop)
	}
	done := c.peerMgmtDone
	c.peerMgmtLock.Unlock()
	if done != nil {
		select {
		case <-time.After(time.Minute):
		case <-done:
		}
	}
	for _, p := range c.currentPeers() {
		xio.CloseIgnoringErrors(p.conn)
	}
}

// startPeerManagement starts the goroutine that manages our peers and does not return until it has made its initial
// adjustment.
func (c *Client) startPeerManagement() {
	ready := make(chan struct{})
	done := make(chan struct{})
	c.peerMgmtLock.Lock()
	c.peerMgmtDone = done
	c.peerMgmtLock.Unlock()
	// Tickers, rather than a fresh timer on each pass through the loop, since restarting the timers every time around
	// would mean the longer of the two intervals could never elapse.
	clearExpired := time.NewTicker(peerClearExpiredDownloadsInterval)
	adjust := time.NewTicker(peerAdjustmentInterval)
	go func() {
		defer clearExpired.Stop()
		defer adjust.Stop()
		c.managePeers(ready, done, clearExpired.C, adjust.C)
	}()
	<-ready
}

// managePeers periodically adjusts our peers until the client is stopped. Expired downloads are cleared each time
// 'clearExpired' ticks and peers are adjusted each time 'adjust' ticks. 'ready' is closed once the initial adjustment
// has been made and 'done' is closed once peer management has stopped. Note that c.peerMgmtStop is only ever closed,
// never replaced, so it may be used without holding the lock.
func (c *Client) managePeers(ready, done chan struct{}, clearExpired, adjust <-chan time.Time) {
	defer close(done)
	select {
	case <-c.peerMgmtStop:
		// The client was stopped before we started, so don't connect to any peers
	default:
		c.adjustPeers()
	}
	close(ready)
	for {
		select {
		case <-clearExpired:
			for _, p := range c.currentPeers() {
				p.clearExpiredDownloads()
			}
		case <-adjust:
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

// worseForRotation returns true if this peer should be given up before the other one when a connection has to be
// dropped to make room for an alternate.
//
// Each key settles the comparison whenever the two peers differ on it, rather than only being able to answer in one
// direction. Letting a lower-priority key answer for a pair that a higher-priority one has already decided would make
// each of the two peers rank ahead of the other, which is not an ordering at all and leaves the sorted result
// arbitrary. Note also that the byte counts come from the state snapshot taken under the peer's lock rather than from
// the peer itself, which its own goroutines are concurrently updating.
func (d *peerData) worseForRotation(other *peerData, now time.Time) bool {
	if d.state.amInterested != other.state.amInterested {
		return !d.state.amInterested
	}
	if d.state.downloading != other.state.downloading {
		return !d.state.downloading
	}
	if d.state.peerChoking != other.state.peerChoking {
		return d.state.peerChoking
	}
	stalled := d.state.downloadStalled(now)
	if otherStalled := other.state.downloadStalled(now); stalled != otherStalled {
		return stalled
	}
	if d.state.bytesRead != other.state.bytesRead {
		return d.state.bytesRead < other.state.bytesRead
	}
	return d.peer.created.After(other.peer.created)
}

// betterForUnchoking returns true if this peer should be preferred over the other one when deciding which peers to
// unchoke. As with worseForRotation, each key settles the comparison whenever the two peers differ on it.
func (d *peerData) betterForUnchoking(other *peerData, now time.Time) bool {
	if d.state.amInterested != other.state.amInterested {
		return d.state.amInterested
	}
	if d.state.downloading != other.state.downloading {
		return d.state.downloading
	}
	if d.state.peerChoking != other.state.peerChoking {
		return !d.state.peerChoking
	}
	if d.state.peerInterested != other.state.peerInterested {
		return d.state.peerInterested
	}
	stalled := d.state.downloadStalled(now)
	if otherStalled := other.state.downloadStalled(now); stalled != otherStalled {
		return !stalled
	}
	if d.state.bytesRead != other.state.bytesRead {
		return d.state.bytesRead > other.state.bytesRead
	}
	if d.state.bytesWritten != other.state.bytesWritten {
		return d.state.bytesWritten > other.state.bytesWritten
	}
	return d.peer.created.After(other.peer.created)
}

// worseForDropping returns true if this peer should be dropped before the other one when room has to be made for an
// incoming connection. Only peers that are neither downloading nor of any interest to us are considered for this, so
// the keys that would separate those cases aren't needed here.
func (d *peerData) worseForDropping(other *peerData) bool {
	if d.state.peerChoking != other.state.peerChoking {
		return d.state.peerChoking
	}
	if d.state.bytesRead != other.state.bytesRead {
		return d.state.bytesRead < other.state.bytesRead
	}
	return d.peer.created.After(other.peer.created)
}

// hostsInUse returns the set of hosts we already have a connection to, either through a peer that has been registered
// or through an outgoing connection attempt that hasn't finished yet.
func (c *Client) hostsInUse(pd []*peerData) map[string]bool {
	existing := make(map[string]bool, len(pd))
	for _, one := range pd {
		if host, _, err := net.SplitHostPort(one.peer.conn.RemoteAddr().String()); err == nil {
			existing[host] = true
		}
	}
	c.lock.RLock()
	maps.Copy(existing, c.dialing)
	c.lock.RUnlock()
	return existing
}

// startDial records that an outgoing connection attempt to the host is under way, returning false if there already is
// one. The attempt has no peer registered for it until the handshake exchange has completed, which together with the
// dial can take longer than the interval between peer adjustments, so an attempt that wasn't recorded here would be
// made a second time by the next adjustment, wasting a peer slot on each side with a duplicate connection that the
// remote then drops.
func (c *Client) startDial(host string) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.dialing[host] {
		return false
	}
	c.dialing[host] = true
	return true
}

// finishDial records that the outgoing connection attempt to the host is over, making the host available again.
func (c *Client) finishDial(host string) {
	c.lock.Lock()
	delete(c.dialing, host)
	c.lock.Unlock()
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
		// Peers that are choking us won't be sending us anything, but that is normal protocol behavior, so they are
		// simply not counted as downloading. Only a peer that is free to send us data and has failed to do so within
		// the allowed time is considered to be at fault.
		if data.state.downloading && !data.state.peerChoking {
			if data.state.downloadStalled(now) {
				c.dispatcher.GateKeeper().BlockAddress(data.peer.conn.RemoteAddr())
				xio.CloseIgnoringErrors(data.peer.conn)
				continue
			}
			downloadCount++
		}
		pd = append(pd, data)
	}
	slog.Debug("managing peers", "download_count", downloadCount, "seeding_complete", c.tracker.isSeedingComplete())
	if downloadCount < c.concurrentDownloads && !c.tracker.isSeedingComplete() {
		existing := c.hostsInUse(pd)
		count := min(c.peersWanted-len(pd), 4)
		if count < 1 && len(pd) > 0 {
			// Find one to disconnect so we can add an alternate
			sort.Slice(pd, func(i, j int) bool { return pd[i].worseForRotation(pd[j], now) })
			xio.CloseIgnoringErrors(pd[0].peer.conn)
			pd = pd[1:]
			count = 1
		}
		slog.Debug("managing peers", "wanted", count)
		if count > 0 {
			peerAddressMap := c.tracker.peerAddressesMap()
			pam := make(map[string]int, len(peerAddressMap))
			for addr, port := range peerAddressMap {
				blocked := c.dispatcher.GateKeeper().IsAddressStringBlocked(addr)
				slog.Debug("managing peers", "address", addr, "port", port, "exists", existing[addr],
					"blocked", blocked)
				if _, exists := existing[addr]; !exists && !blocked {
					pam[addr] = port
				}
			}
			slog.Debug("managing peers", "available", len(pam))
			for i := 0; i < count; i++ {
				added := false
				for addr, port := range pam {
					if !existing[addr] && !c.dispatcher.GateKeeper().IsAddressStringBlocked(addr) &&
						c.startDial(addr) {
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
	sort.Slice(pd, func(i, j int) bool { return pd[i].betterForUnchoking(pd[j], now) })
	for i, one := range pd {
		one.peer.setChoked(i > 3 && !one.state.downloading)
	}
}

func (c *Client) dropPeerIfPossible() bool {
	peers := c.currentPeers()
	pd := make([]*peerData, 0, len(peers))
	for _, p := range peers {
		state := p.updateInterest()
		if !state.downloading && !state.amInterested {
			pd = append(pd, &peerData{peer: p, state: state})
		}
	}
	switch len(pd) {
	case 0:
		return false
	case 1:
	default:
		sort.Slice(pd, func(i, j int) bool { return pd[i].worseForDropping(pd[j]) })
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

// informPeersWeHavePiece records the piece for advertisement to each of our peers. This is called from the read
// goroutine of the peer that completed the piece, so it only records what has to be sent and leaves the delivery to
// each peer's own goroutine, rather than letting a single peer whose write queue has backed up stall it.
func (c *Client) informPeersWeHavePiece(index int) {
	for _, one := range c.currentPeers() {
		one.queueHave(index)
	}
}
