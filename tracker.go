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
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/container/fixedbits"
	"github.com/richardwilkes/torrent/tio"
	"github.com/zeebo/bencode"
)

const (
	startedMsg   = "started"
	stoppedMsg   = "stopped"
	completedMsg = "completed"

	// minAnnounceInterval and maxAnnounceInterval bound how long we wait between announces. The tracker's requested
	// interval is only a request, and it is unverified data: too small a value has us hammering the tracker, while one
	// large enough to overflow the conversion to a time.Duration yields a negative delay whose timer fires
	// immediately, turning the periodic announce into a tight loop of HTTP round trips.
	minAnnounceInterval = 5 * time.Minute
	maxAnnounceInterval = 24 * time.Hour

	// maxTrackerResponseSize is the largest tracker response we'll read. Even a response listing thousands of peers is
	// a few tens of kilobytes, so this is generous while still bounding what an unfriendly tracker can hand us.
	maxTrackerResponseSize = 1024 * 1024

	// maxBencodeDepth is the deepest nesting of lists and dictionaries accepted in a tracker response. The decoder
	// recurses for each level, so an unbounded depth is a way to run us out of stack with very little data.
	maxBencodeDepth = 16

	// maxPeerListOverage is how many times the number of peers we asked for a tracker may answer with before the rest
	// of its list is dropped. Every announce says how many we want, and nothing obliges the tracker to honor it: a
	// megabyte of compact entries, which the response cap allows, is about 174,000 peers, each of them retained for
	// the life of the torrent and walked — with a gatekeeper lookup and a log line apiece — on every fifteen-second
	// peer adjustment. Several times what we asked for is room enough for a tracker that rounds our request up or
	// hands out its whole small swarm, while what it can't be is unbounded.
	maxPeerListOverage = 4

	// stopAnnounceWait is how long the shutdown will wait for the periodic announce goroutine to finish. The request
	// that goroutine may be in the middle of is aborted before the wait begins, so this is only reached if the
	// transport doesn't unwind from that promptly; the shutdown then goes ahead rather than being held up further.
	stopAnnounceWait = 5 * time.Second
)

var (
	// trackerUserAgent is read by every announce, which are made from the client's run goroutine and from each
	// tracker's periodic announce goroutine, and may be set by the application at any point. A plain string would make
	// any set that isn't strictly ordered before every client start a data race, which is not a constraint an
	// application can be expected to honor for a package-level knob.
	trackerUserAgent atomic.Pointer[string]
	httpClient       = &http.Client{Timeout: 30 * time.Second}
)

// SetTrackerUserAgent sets the user agent header sent with tracker announces. An empty string, which is the default,
// sends none. It is safe to call at any time, including while clients are announcing.
func SetTrackerUserAgent(agent string) {
	trackerUserAgent.Store(&agent)
}

// TrackerUserAgent returns the user agent header sent with tracker announces. An empty string means none is sent.
func TrackerUserAgent() string {
	if agent := trackerUserAgent.Load(); agent != nil {
		return *agent
	}
	return ""
}

type tracker struct {
	client *Client
	// announceCtx is canceled to tell the periodic announce goroutine to stop. A context rather than a channel because
	// that goroutine may be in the middle of an HTTP announce, which is bounded only by the client's 30 second
	// timeout: cancellation reaches the request it is parked on, so the shutdown can wait for the goroutine — which it
	// must, or the update it is making would be in flight alongside the stopped event that follows — without the
	// stopped announce, the close of the storage file and the stopped notification queuing behind a full round trip.
	announceCtx    context.Context
	cancelAnnounce context.CancelFunc
	// completeAnnounceRequested carries the "completed" announce from the peer goroutine that finished the download to
	// the periodic announce goroutine, which is the only place an announce is made from once we've started. Buffered by
	// one and never blocking on a send, since a request already pending says everything a second one would.
	completeAnnounceRequested chan struct{}
	trackerLockData
	// The transfer totals are reported in every announce and are updated by each peer's read and write goroutines, so
	// they are kept as atomics rather than under the tracker lock, which those hot paths would otherwise contend on
	// for every message.
	uploadedBytes   atomic.Int64
	downloadedBytes atomic.Int64
	lock            sync.RWMutex
}

type trackerLockData struct {
	have        *fixedbits.Bits
	downloading *fixedbits.Bits
	who         map[int]*peer
	// periodicAnnounceDone is closed by the periodic announce goroutine as it returns, which is what the shutdown
	// waits on. It stays nil while no such goroutine exists, so a tracker whose start announce failed has nothing to
	// wait for.
	periodicAnnounceDone chan struct{}
	seedExpires          time.Time
	trackerID            string
	peerAddresses        []peerAddr
	currentState         State
	totalBytes           int64
	remainingBytes       int64
	interval             int
	leechers             int
	seeders              int
	progress             float64
	started              bool
	// inSwarm is set once peers can reach us, which is what separates a download this session earned from one that was
	// already sitting on disk when we started. Only the former has a "completed" event to report.
	inSwarm bool
	// completePending records a completion that arrived before the start announce had been answered, which is the
	// window between the dispatcher registration and the tracker's reply — up to the HTTP client's timeout, and long
	// enough for an inbound peer to deliver the last pieces of a nearly complete torrent. announceStart hands it over
	// once there is both a started swarm to report it to and a goroutine to report it from.
	completePending bool
}

type trackerWire struct { //nolint:govet // We can't change the order of these fields
	Interval int `bencode:"interval"`
	// PeerAddresses is left as raw bencode because a tracker may answer with either the compact form (a string) or the
	// dict model (a list of dictionaries), regardless of our request for the compact form.
	PeerAddresses bencode.RawMessage `bencode:"peers"`
	// PeerAddresses6 is BEP-7's list of IPv6 peers, which this client doesn't speak — it announces and dials IPv4 —
	// but which it has to be able to see. Without the field the key is dropped by the decoder, so a tracker answering
	// with nothing but IPv6 peers leaves the client with no one to talk to and nothing said about why; with it, the
	// announce reports what it was offered and couldn't use.
	PeerAddresses6 bencode.RawMessage `bencode:"peers6"`
	Seeders        int                `bencode:"complete"`
	Leechers       int                `bencode:"incomplete"`
	TrackerID      string             `bencode:"tracker id"`
	Failure        string             `bencode:"failure reason"`
}

// peerWire is one entry of a tracker's dict-model peer list.
type peerWire struct { //nolint:govet // We can't change the order of these fields
	ID   string `bencode:"peer id"`
	IP   string `bencode:"ip"`
	Port int    `bencode:"port"`
}

func newTracker(client *Client) *tracker {
	totalBytes := client.torrentFile.Size()
	totalPieces := client.torrentFile.PieceCount()
	ctx, cancel := context.WithCancel(context.Background())
	return &tracker{
		client:                    client,
		announceCtx:               ctx,
		cancelAnnounce:            cancel,
		completeAnnounceRequested: make(chan struct{}, 1),
		trackerLockData: trackerLockData{
			totalBytes:     totalBytes,
			remainingBytes: totalBytes,
			have:           fixedbits.New(totalPieces),
			downloading:    fixedbits.New(totalPieces),
			who:            make(map[int]*peer),
		},
	}
}

// addDownloadedBytes records data received from a peer for this torrent, which is reported to the tracker in every
// announce. Everything read from a peer is counted, since all of it was transferred on this torrent's behalf.
func (t *tracker) addDownloadedBytes(count int64) {
	t.downloadedBytes.Add(count)
}

// addUploadedBytes records data sent to a peer for this torrent, which is reported to the tracker in every announce.
// Everything written to a peer is counted, since all of it was transferred on this torrent's behalf.
func (t *tracker) addUploadedBytes(count int64) {
	t.uploadedBytes.Add(count)
}

// markBlockValid records a piece we now hold. A repeat call for an index we already have changes nothing and says
// nothing: the "have" message goes out with the rest of the state change, inside the guard, so that a piece claimed
// twice — which the claim bookkeeping makes unlikely rather than impossible — doesn't queue a second announcement of
// it to every peer we have.
func (t *tracker) markBlockValid(index int) {
	fresh := false
	announce := false
	t.lock.Lock()
	if !t.have.IsSet(index) {
		fresh = true
		t.have.Set(index)
		t.downloading.Unset(index)
		delete(t.who, index)
		if t.remainingBytes -= t.client.torrentFile.LengthOf(index); t.remainingBytes <= 0 {
			t.remainingBytes = 0
			t.seedExpires = time.Now().Add(t.client.seedDuration)
			announce = true
		}
	}
	t.lock.Unlock()
	if !fresh {
		return
	}
	t.client.informPeersWeHavePiece(index)
	if announce {
		// The piece that finishes the download is what starts the seeding clock, and the run goroutine is waiting on a
		// deadline that didn't exist until now, so it is told to look again.
		t.client.wakeRun()
		t.requestCompleteAnnounce()
	}
}

// enterSwarm records that peers can now reach us, which is what makes everything downloaded from here on something
// this session earned. It is called as the client registers with the dispatcher, before the start announce, since an
// inbound peer can deliver pieces from that moment and the announce it would report them through may still be in
// flight.
func (t *tracker) enterSwarm() {
	t.lock.Lock()
	t.inSwarm = true
	t.lock.Unlock()
}

// requestCompleteAnnounce hands the "completed" announce to the periodic announce goroutine rather than making it
// here. This runs on a peer's message loop, which the client's peer wait group tracks, so making the announce here
// stalls that peer for a full tracker round trip and, when a stop lands while it is in flight, holds up the shutdown
// — the stopped announce, the close of the storage file and the stopped notification all queue behind the wait for
// that peer — for as long as the HTTP client's timeout allows, well past the bound the shutdown otherwise keeps every
// announce within. A download that finished before we ever announced a start, which is what verifying an already
// complete file on disk does, has no completion to report and no goroutine to report it: the event describes a
// torrent downloaded during this session.
//
// A completion that lands after we joined the swarm but before the start announce has been answered is that kind of
// event and is recorded rather than dropped, since the peer that delivered it is one this session took the pieces
// from. Nothing re-checks afterwards on its own: the goroutine that services the request only starts with the
// periodic announce, and it drains the channel from that point on, so a request made now would be seen while one
// merely dropped would not.
func (t *tracker) requestCompleteAnnounce() {
	t.lock.Lock()
	if !t.started {
		t.completePending = t.inSwarm
		t.lock.Unlock()
		return
	}
	t.lock.Unlock()
	select {
	case t.completeAnnounceRequested <- struct{}{}:
	default:
	}
}

// servicePendingCompleteAnnounce hands over a completion that arrived while the start announce was still in flight.
// The request is queued rather than announced here: this runs on the client's run goroutine, which the rest of the
// startup is waiting on, and the periodic announce goroutine is where every announce after the start is made from.
func (t *tracker) servicePendingCompleteAnnounce() {
	t.lock.Lock()
	pending := t.completePending
	t.completePending = false
	t.lock.Unlock()
	if !pending {
		return
	}
	select {
	case t.completeAnnounceRequested <- struct{}{}:
	default:
	}
}

// peerListLimit returns how many peers an announce may take from a tracker's answer. It is a multiple of what we asked
// for rather than a fixed number, since the request is what the tracker was told we wanted.
func (t *tracker) peerListLimit() int {
	return t.client.peersWanted * maxPeerListOverage
}

// knownPeers returns the peers the last announce told us about.
func (t *tracker) knownPeers() []peerAddr {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.peerAddresses
}

func (t *tracker) setStateAndProgress(state State, progress float64) {
	t.lock.Lock()
	t.currentState = state
	if progress < 0 {
		t.progress = float64(t.totalBytes-t.remainingBytes) * 100 / float64(t.totalBytes)
	} else {
		t.progress = progress
	}
	t.lock.Unlock()
}

func (t *tracker) setState(state State) {
	t.lock.Lock()
	seedingTransition := t.currentState != Seeding && state == Seeding
	t.currentState = state
	t.lock.Unlock()
	if seedingTransition {
		t.client.notifyDownloadComplete()
	}
}

func (t *tracker) isDownloadComplete() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.remainingBytes <= 0
}

func (t *tracker) isSeedingComplete() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.remainingBytes <= 0 && time.Now().After(t.seedExpires)
}

// seedingDeadline returns when seeding is due to be over and whether there is such a deadline yet. There isn't one
// until the download is complete, since that is what starts the clock.
func (t *tracker) seedingDeadline() (deadline time.Time, ok bool) {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.seedExpires, t.remainingBytes <= 0
}

func (t *tracker) setProgress(progress float64) {
	t.lock.Lock()
	if progress < 0 {
		t.progress = float64(t.totalBytes-t.remainingBytes) * 100 / float64(t.totalBytes)
	} else {
		t.progress = progress
	}
	t.lock.Unlock()
}

func (t *tracker) status(peersDownloading, peersConnected int) *Status {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return &Status{
		State:                  t.currentState,
		PercentComplete:        t.progress,
		TotalBytes:             t.totalBytes,
		RemainingBytes:         t.remainingBytes,
		UploadBytesPerSecond:   t.client.OutRate.LastUsed(),
		DownloadBytesPerSecond: t.client.InRate.LastUsed(),
		PeersDownloading:       peersDownloading,
		PeersConnected:         peersConnected,
		Leechers:               t.leechers,
		Seeders:                t.seeders,
		SeedingStopsAt:         t.seedExpires,
	}
}

func (t *tracker) hasStarted() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.started
}

// announceStart makes the announce that puts us in the swarm and, if it succeeds, starts the periodic announce.
//
// It is made with the context the stop cancels, like every announce that follows it. Left uncancelable, a Stop that
// arrives while this is parked on a slow or unresponsive tracker can't cut it short the way it can every later
// announce, so run() stays blocked here for up to the HTTP client's 30 second timeout and the stopped announce, the
// close of the storage file and the stopped notification all land well past the bound the caller asked for. Being cut
// short that way isn't a failure to report: the stop is what asked for it, so it is reported as the requested stop it
// is, which is what keeps a torrent shut down while it was starting from being recorded as one that errored.
func (t *tracker) announceStart() error {
	if t.hasStarted() {
		return nil
	}
	if err := t.announce(t.announceCtx, startedMsg); err != nil {
		if t.announceStopping() {
			return errStopRequested
		}
		return err
	}
	if t.isDownloadComplete() {
		t.setState(Seeding)
	} else {
		t.setStateAndProgress(Downloading, -1)
	}
	t.startPeriodicAnnounce()
	// A download that finished while this announce was in flight had nowhere to send its completion: the tracker had
	// no record of us to report it to, and the goroutine that makes that announce didn't exist. Both hold now.
	t.servicePendingCompleteAnnounce()
	return nil
}

// announceComplete reports the finished download. It is made with the context the stop cancels, like every other
// announce the periodic goroutine makes, so that one caught in the middle of a round trip unwinds at once instead of
// holding the shutdown that is waiting for that goroutine.
func (t *tracker) announceComplete() error {
	if !t.hasStarted() {
		return nil
	}
	return t.announce(t.announceCtx, completedMsg)
}

func (t *tracker) announceStopped() error {
	// The periodic announce is stopped whether or not we ever managed to start, so that nothing is left running, and
	// nothing left waiting to be canceled, for a client that is on its way out.
	t.stopPeriodicAnnounce()
	if !t.hasStarted() {
		return nil
	}
	// Deliberately not the announce context, which the stop above has just canceled.
	return t.announce(context.Background(), stoppedMsg)
}

// startPeriodicAnnounce starts the goroutine that announces at the interval the tracker asked for. The channel the
// stop waits on is recorded before the goroutine exists, so that a stop arriving immediately afterwards can't find
// nothing to wait for.
func (t *tracker) startPeriodicAnnounce() {
	done := make(chan struct{})
	t.lock.Lock()
	t.periodicAnnounceDone = done
	t.lock.Unlock()
	go func() {
		defer close(done)
		t.periodicAnnounce()
	}()
}

// stopPeriodicAnnounce tells the periodic announce goroutine to stop, whether it is waiting for its next turn or in
// the middle of one, and waits for it to finish. Without the wait, the update it is making would still be in flight,
// on a connection of its own, while the stopped announce went out: a tracker that finished with the update after the
// stopped event would have us back in its swarm for a full announce interval after we shut down, and the response to
// it would land on tracker state that no longer describes anything. The wait is short because the announce is
// canceled rather than left to run out the HTTP timeout, and bounded regardless, since the stopped announce, the close
// of the storage file and the stopped notification all queue behind it. A goroutine that never ran, because the start
// announce failed, simply has nothing to wait for.
func (t *tracker) stopPeriodicAnnounce() {
	t.cancelAnnounce()
	t.lock.RLock()
	done := t.periodicAnnounceDone
	t.lock.RUnlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(stopAnnounceWait):
		t.client.logger.Warn("timed out waiting for the periodic announce to stop", "timeout", stopAnnounceWait)
	}
}

// announceStopping returns true once the shutdown has told the periodic announce to stop.
func (t *tracker) announceStopping() bool {
	return t.announceCtx.Err() != nil
}

// announceInterval returns how long to wait before the next announce. The tracker's requested interval is bounded at
// both ends: it is unverified data, and the conversion to a time.Duration is only safe once the value is known to be
// small enough not to overflow it. Non-positive values are answered before that conversion is made at all, since a
// large enough negative one overflows it just as a large positive one does, and lands back in the positive range: an
// interval of about 292 years, which would silence the periodic announce for the life of the process.
func (t *tracker) announceInterval() time.Duration {
	t.lock.RLock()
	seconds := t.interval
	t.lock.RUnlock()
	if seconds <= 0 {
		return minAnnounceInterval
	}
	if seconds > int(maxAnnounceInterval/time.Second) {
		return maxAnnounceInterval
	}
	if interval := time.Duration(seconds) * time.Second; interval > minAnnounceInterval {
		return interval
	}
	return minAnnounceInterval
}

// periodicAnnounce makes the announces that follow the start: the ones the tracker's interval asks for, and the
// "completed" one a peer asks for when the last piece lands. Both are made here so that every announce after the start
// is on this one goroutine, which is the one the shutdown cancels and waits for. Servicing a completion restarts the
// interval as well, since the tracker has just been brought up to date by an announce of its own.
func (t *tracker) periodicAnnounce() {
	for {
		timer := time.After(t.announceInterval())
		select {
		case <-timer:
			// The announce is made with the context the stop cancels, so that one caught in the middle of a round trip
			// unwinds at once rather than holding up the shutdown that is waiting for this goroutine. Nothing is
			// logged for that: being cut short is what was asked for, not a failure to report.
			if err := t.announce(t.announceCtx, ""); !t.announceStopping() && tio.ShouldLogIOError(err) {
				errs.LogTo(t.client.logger, err)
			}
		case <-t.completeAnnounceRequested:
			if err := t.announceComplete(); !t.announceStopping() && tio.ShouldLogIOError(err) {
				errs.LogTo(t.client.logger, err)
			}
		case <-t.announceCtx.Done():
			return
		}
	}
}

// peerAddr is one entry of a tracker's peer list.
type peerAddr struct {
	ip   string
	port int
}

// isSelf returns true if this entry is the address we announced ourselves under. Both halves have to match: a swarm
// commonly has several peers behind one NAT, and dropping every entry whose IP equals our external address discards
// those peers rather than only us. An unknown external address matches nothing, which is what the caller passes when
// the lookup didn't succeed.
func (a peerAddr) isSelf(externalAddr string, externalPort int) bool {
	return a.ip == externalAddr && a.port == externalPort
}

// isDialable returns true if the entry names a host that could be a peer. The port is left to the callers, which read
// it from formats whose ranges differ; what is checked here is the host, which the compact form always fills in but
// which the dict model takes verbatim from the tracker.
//
// A peer list is unverified data, and whoever supplies a .torrent chooses both the tracker it announces to and the
// peers that tracker hands back, so an entry is an address of the tracker's choosing that we open a TCP connection to
// and write a 68 byte handshake carrying 20 bytes it also chose. What is refused here are the addresses that name
// something other than a peer somewhere out on the network:
//
//   - No host at all, which makes net.JoinHostPort produce ":6881", and the unspecified address, which produces
//     "0.0.0.0:6881". Either dials our own machine, so what receives the handshake is whatever local service happens
//     to be listening on the port the tracker chose.
//   - Loopback, so that the whole of 127.0.0.0/8 — every port of it a distinct key, and so unthrottled by the
//     gatekeeper's per-address block — can't be swept for services that don't expect to hear from the network at all.
//   - Link-local, multicast and the IPv4 broadcast address, none of which a TCP peer can live at.
//   - 0.0.0.0/8, "this network", which is no destination either. It is also where much of what a compact IPv6 list
//     misread as IPv4 entries lands, since the addresses that list holds are mostly zero bytes.
//
// Private addresses are deliberately left dialable: a swarm on a LAN, announced to a tracker on that same LAN, is an
// ordinary arrangement rather than an attack, and refusing them here would break it. Anything that isn't an address at
// all is left alone too: BEP 3 lets the dict model carry a DNS name, and resolving it is the dial's business.
func (a peerAddr) isDialable() bool {
	if a.ip == "" {
		return false
	}
	ip := net.ParseIP(a.ip)
	if ip == nil {
		return true
	}
	// IsGlobalUnicast is false for exactly the unspecified, loopback, link-local, multicast and broadcast addresses,
	// and true for the private ranges, which is the division wanted here.
	if !ip.IsGlobalUnicast() {
		return false
	}
	v4 := ip.To4()
	return v4 == nil || v4[0] != 0
}

// parseCompactPeers extracts the peer addresses from the compact peer list format, which is a series of 6-byte
// entries, each holding a 4-byte IPv4 address followed by a 2-byte port. A tracker response is unverified data, so a
// trailing partial entry is ignored rather than allowed to run off the end of the list — parsePeers refuses a list of
// such a length outright, but this stays safe whatever it is handed — and no more than limit entries are returned, so
// that neither the list built here nor the walk every peer adjustment makes over it is decided by the tracker. Our
// own address is omitted, as are entries with no port and entries naming no host.
func parseCompactPeers(value, externalAddr string, externalPort, limit int) []peerAddr {
	peerAddresses := make([]peerAddr, 0, min(len(value)/6, limit))
	for i := 0; i+6 <= len(value) && len(peerAddresses) < limit; i += 6 {
		one := peerAddr{
			ip:   net.IPv4(value[i], value[i+1], value[i+2], value[i+3]).String(),
			port: int(binary.BigEndian.Uint16([]byte(value[i+4 : i+6]))),
		}
		if one.port != 0 && one.isDialable() && !one.isSelf(externalAddr, externalPort) {
			peerAddresses = append(peerAddresses, one)
		}
	}
	return peerAddresses
}

// parsePeers extracts the peer addresses from a tracker's "peers" value. Although we always ask for the compact form,
// a tracker is free to ignore that and answer with the dict model instead, so both are accepted: a bencoded string is
// the compact form and a bencoded list is the dict model. A missing or empty value simply yields no peers. Our own
// address is omitted, as are entries whose port or host isn't one that can be dialed. At most limit entries are kept,
// since the announce says how many peers we want but nothing makes the tracker honor it, and everything downstream —
// the list held per torrent, and the walk over it every peer adjustment makes — is sized by what comes back. What was
// found, and anything dropped for the limit, is reported to the caller's logger, since a peer list is per-torrent
// detail that belongs wherever the rest of that client's logging goes.
//
// The result is a list rather than a map keyed by IP, since several peers of a swarm sharing one IP is the ordinary
// state of affairs behind a NAT: keyed that way, all but the last of them are lost.
func parsePeers(logger *slog.Logger, raw bencode.RawMessage, externalAddr string, externalPort, limit int) ([]peerAddr, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	switch {
	case raw[0] >= '0' && raw[0] <= '9': // A bencoded string, so the compact form
		var compact string
		if err := bencode.DecodeBytes(raw, &compact); err != nil {
			return nil, errs.NewWithCause("unable to decode compact peer list", err)
		}
		logger.Debug("announce string", "peers_list", compact)
		// A compact list is whole 6-byte entries or it is not a compact list. Refusing a length that isn't says so
		// rather than quietly keeping whatever prefix happens to divide evenly, which is the shape a response takes
		// when it was truncated or when it holds something other than what BEP-23 says it holds. What this cannot
		// catch is BEP-7's 18-byte IPv6 entries put in "peers" instead of "peers6": 18 divides by 6, so such a list
		// looks exactly like three times as many IPv4 entries. Those entries are mostly zero bytes, so isDialable
		// discards the bulk of what they misparse into, and a tracker that has IPv6 peers to offer is expected to
		// offer them under the key BEP-7 gives them, which is answered below.
		if len(compact)%6 != 0 {
			return nil, errs.Newf("compact peer list of %d bytes is not a whole number of 6 byte entries", len(compact))
		}
		if entries := len(compact) / 6; entries > limit {
			logger.Warn("tracker offered more peers than allowed", "offered", entries, "limit", limit)
		}
		return parseCompactPeers(compact, externalAddr, externalPort, limit), nil
	case raw[0] == 'l': // A bencoded list, so the dict model
		var list []peerWire
		if err := bencode.DecodeBytes(raw, &list); err != nil {
			return nil, errs.NewWithCause("unable to decode peer list", err)
		}
		logger.Debug("announce map", "count", len(list))
		if len(list) > limit {
			logger.Warn("tracker offered more peers than allowed", "offered", len(list), "limit", limit)
		}
		peerAddresses := make([]peerAddr, 0, min(len(list), limit))
		for _, one := range list {
			if len(peerAddresses) == limit {
				break
			}
			// The dict model carries the port as a bencoded integer, so unlike the compact form — whose ports are
			// inherently 16 bit — a hostile or buggy tracker can hand us a negative one or one past the end of the port
			// range. Kept, they become dial attempts that cannot possibly succeed, retried on every peer management
			// pass until the next announce replaces the list.
			if one.Port > 0 && one.Port <= math.MaxUint16 {
				entry := peerAddr{ip: one.IP, port: one.Port}
				if entry.isDialable() && !entry.isSelf(externalAddr, externalPort) {
					peerAddresses = append(peerAddresses, entry)
				}
			}
		}
		return peerAddresses, nil
	default:
		return nil, errs.Newf("unknown peer address format: %q", raw[0])
	}
}

func (t *tracker) announce(ctx context.Context, event string) error {
	urlStr := t.announceURL(event)
	t.client.logger.Debug("announce", "url", urlStr)
	resp, err := t.request(ctx, urlStr)
	if err != nil {
		return err
	}
	defer discardAndCloseAnnounceBody(resp.Body)
	switch event {
	case startedMsg:
		// The tracker has us in its swarm from the moment it answers this request, whatever the response then turns
		// out to say. Recording that only once the response has been made sense of leaves a start that failed on a
		// missing interval or an unreadable peer list registered with a tracker that will never be told we left, so it
		// goes on handing our dead address to peers until its own timeout runs out. A stopped event for a tracker that
		// had no record of us — one that answered with a failure reason, say — costs nothing; one we owed and never
		// sent costs the swarm.
		t.lock.Lock()
		t.started = true
		t.lock.Unlock()
	case stoppedMsg:
		// Nothing in the response to a shutdown announce is ever used: the stop was delivered by the request itself,
		// which the tracker has now answered. Trackers commonly reply to it with an empty body, plain text, or a status
		// other than 200, precisely because no one reads it, so holding the response to the same standard as every
		// other announce only produces a spurious error on the way out and leaves us believing we are still started.
		// A request that never reached the tracker is a different matter and is still reported by t.request above.
		t.lock.Lock()
		t.started = false
		t.lock.Unlock()
		t.client.logger.Info("announce", "event", stoppedMsg)
		return nil
	}
	in, err := readAnnounceResponse(resp)
	if err != nil {
		return err
	}
	if in.Failure != "" {
		return errs.New(in.Failure)
	}
	// Only the start announce needs an interval of its own, since it is what launches the periodic announce. For the
	// rest, a tracker that omits one just leaves the interval already in hand governing the next announce.
	if in.Interval < 1 && event == startedMsg {
		return errs.New("invalid interval")
	}
	externalAddr := "<unknown>"
	if extIP := t.client.ExternalIP(); extIP != nil {
		externalAddr = extIP.String()
	}
	peerAddresses, err := parsePeers(t.client.logger, in.PeerAddresses, externalAddr,
		int(t.client.dispatcher.ExternalPort()), t.peerListLimit())
	if err != nil {
		return err
	}
	// Said out loud rather than passed over: a tracker answering an IPv6-capable swarm may put every peer it has under
	// this key, and a client that simply drops it looks like one the swarm has nothing for.
	if len(in.PeerAddresses6) != 0 {
		t.client.logger.Warn("ignoring the IPv6 peer list, which this client doesn't implement (BEP-7)",
			"bytes", len(in.PeerAddresses6), "ipv4_peers", len(peerAddresses))
	}
	// An announce the shutdown overtook has nothing left to say. What it came back with describes a swarm we are on
	// our way out of, and letting the peer list, the interval and the counts we report land after the stopped event
	// would leave all of them belonging to a client that no longer exists.
	//
	// The start announce is exempt, and not because a stop can't overlap it: Client.Stop cancels this very context from
	// another goroutine precisely so that a start announce parked on a slow tracker is cut short, so a response that
	// beats the cancellation is applied with the stop already under way. It is exempt because it is the announce the
	// rest of the startup is built on — announceStart goes on to start the periodic announce at the interval recorded
	// below, which is the one value a start announce is required to carry — and because applying it late costs nothing.
	// What the shutdown owes the tracker was settled above, by the started flag recorded the moment the tracker
	// answered, and the peer list and counts go down with the client that is on its way out.
	if event != startedMsg && t.announceStopping() {
		return nil
	}
	t.lock.Lock()
	if in.Interval > 0 {
		t.interval = in.Interval
	}
	// A tracker id, once issued, is ours to send back on every announce that follows, and a response that simply
	// leaves the key out isn't taking it away. Clearing it there would drop the trackerid parameter from the rest of
	// our announces and lose us the association a tracker that tracks sessions is keeping.
	if in.TrackerID != "" {
		t.trackerID = in.TrackerID
	}
	t.seeders = in.Seeders
	t.leechers = in.Leechers
	// A response that simply leaves the peers key out isn't telling us the swarm is empty, any more than one leaving
	// the interval or the tracker id out is taking those away, so the list already in hand is what dialCandidates goes
	// on offering until an announce actually says otherwise. Cleared unconditionally, a single such response leaves
	// the client unable to replace a lost connection until the next announce, which the interval allows to be as far
	// off as a day. An explicitly empty list is a different statement and does clear it.
	if len(in.PeerAddresses) != 0 {
		t.peerAddresses = peerAddresses
	}
	if event == startedMsg {
		t.started = true
	}
	t.lock.Unlock()
	if event == "" {
		event = "update"
	}
	t.client.logger.Info("announce", "event", event, "seeders", in.Seeders, "leechers", in.Leechers, "peers",
		len(peerAddresses))
	return nil
}

func (t *tracker) announceURL(event string) string {
	var buffer bytes.Buffer
	buffer.WriteString(t.client.torrentFile.Announce)
	if strings.Contains(t.client.torrentFile.Announce, "?") {
		buffer.WriteString("&")
	} else {
		buffer.WriteString("?")
	}
	buffer.WriteString("info_hash=")
	buffer.WriteString(percentEncode(string(t.client.torrentFile.InfoHash[:])))
	buffer.WriteString("&peer_id=")
	buffer.Write(t.client.id[:])
	fmt.Fprintf(&buffer, "&port=%d", t.client.dispatcher.ExternalPort())
	fmt.Fprintf(&buffer, "&uploaded=%d", t.uploadedBytes.Load())
	fmt.Fprintf(&buffer, "&downloaded=%d", t.downloadedBytes.Load())
	t.lock.RLock()
	fmt.Fprintf(&buffer, "&left=%d", t.remainingBytes)
	if t.trackerID != "" {
		fmt.Fprintf(&buffer, "&trackerid=%s", percentEncode(t.trackerID))
	}
	t.lock.RUnlock()
	fmt.Fprintf(&buffer, "&numwant=%d", t.client.peersWanted)
	buffer.WriteString("&compact=1")
	if event != "" {
		fmt.Fprintf(&buffer, "&event=%s", event)
	}
	return buffer.String()
}

// percentEncode escapes every byte outside RFC 3986's unreserved set as %XX, which is what an announce parameter
// carrying arbitrary bytes needs. url.QueryEscape is form encoding: it writes the byte 0x20 as "+" rather than "%20",
// and a tracker that percent-decodes the query string instead of form-decoding it then reconstructs a "+" (0x2B) where
// that byte belongs. Roughly one info hash in thirteen contains a 0x20, so the tracker looks up a hash that isn't ours,
// answers that it has never heard of the torrent, and those torrents cannot announce to it at all. Mainstream clients
// percent-encode every non-unreserved byte for exactly this reason.
func percentEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var buffer strings.Builder
	buffer.Grow(len(s))
	for i := range len(s) {
		switch ch := s[i]; {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			buffer.WriteByte(ch)
		default:
			buffer.WriteByte('%')
			buffer.WriteByte(hexDigits[ch>>4])
			buffer.WriteByte(hexDigits[ch&0x0F])
		}
	}
	return buffer.String()
}

// request makes the announce request and hands back the tracker's answer to it without judging what that answer says:
// only the delivery of the request is decided here, since that is all the shutdown announce needs to know. The caller
// owns the response body. The whole exchange, the reading of that body included, is bounded by the HTTP client's own
// timeout, while the context is what lets an announce the shutdown has overtaken be cut short well ahead of it.
func (t *tracker) request(ctx context.Context, urlStr string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, http.NoBody)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	if agent := TrackerUserAgent(); agent != "" {
		req.Header.Set("user-agent", agent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return resp, nil
}

// discardAndCloseAnnounceBody reads off what is left of a tracker's answer and closes it, taking no more than the cap
// every announce response is held to. Draining is what lets the connection be reused for the next announce, but the
// body is untrusted data: an uncapped drain hands a hostile tracker the paths that never read the body at all — the
// stopped announce, an answer carrying a failure reason, one already refused for being larger than the cap — as a way
// to keep the announce goroutine streaming discarded bytes until the HTTP client's timeout runs out, which is the
// whole of what the cap exists to prevent.
func discardAndCloseAnnounceBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxTrackerResponseSize)) //nolint:errcheck // Nothing to be done about it
	xio.CloseIgnoringErrors(body)
}

// readAnnounceResponse reads and decodes the tracker's answer to an announce.
func readAnnounceResponse(resp *http.Response) (*trackerWire, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, errs.New("unexpected status: " + resp.Status)
	}
	// A tracker's response is untrusted data, so it is read with a cap and checked before the decoder sees it. The
	// decoder allocates a string's declared length before reading it, so a handful of bytes claiming a 2GB string is
	// otherwise all it takes to make us allocate 2GB on every announce.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTrackerResponseSize+1))
	if err != nil {
		return nil, errs.Wrap(err)
	}
	if len(body) > maxTrackerResponseSize {
		return nil, errs.Newf("tracker response is larger than the %d bytes allowed", maxTrackerResponseSize)
	}
	if err = checkBencode(body); err != nil {
		return nil, err
	}
	var in trackerWire
	if err = bencode.DecodeBytes(body, &in); err != nil {
		return nil, errs.Wrap(err)
	}
	return &in, nil
}

// checkBencode walks the structure of bencoded data, rejecting anything the decoder shouldn't be handed. It is not a
// full grammar check — the decoder still does that — but it guarantees the two things the decoder can't defend
// itself against: that no string claims more bytes than are actually present, since the decoder allocates the
// declared length before discovering there is nothing to fill it with, and that the nesting stays shallow enough for
// the decoder's recursion.
func checkBencode(data []byte) error {
	depth := 0
	for pos := 0; pos < len(data); {
		switch b := data[pos]; {
		case b == 'l' || b == 'd':
			depth++
			if depth > maxBencodeDepth {
				return errs.Newf("bencoded data is nested more than %d deep", maxBencodeDepth)
			}
			pos++
		case b == 'e':
			depth--
			if depth < 0 {
				return errs.New("bencoded data has an unmatched end marker")
			}
			pos++
		case b == 'i':
			end := bytes.IndexByte(data[pos+1:], 'e')
			if end < 0 {
				return errs.New("bencoded data has an unterminated integer")
			}
			pos += end + 2
		case b >= '0' && b <= '9':
			colon := bytes.IndexByte(data[pos:], ':')
			if colon < 0 {
				return errs.New("bencoded data has an unterminated string length")
			}
			length, err := strconv.ParseInt(string(data[pos:pos+colon]), 10, 64)
			if err != nil || length < 0 {
				return errs.Newf("bencoded data has an invalid string length: %s", data[pos:pos+colon])
			}
			start := pos + colon + 1
			if length > int64(len(data)-start) {
				return errs.Newf("bencoded data declares a %d byte string, but only %d bytes remain", length,
					len(data)-start)
			}
			pos = start + int(length)
		default:
			return errs.Newf("bencoded data has an unexpected byte: %q", b)
		}
	}
	if depth != 0 {
		return errs.New("bencoded data is truncated")
	}
	return nil
}

// clearDownload gives up the claim the given peer has on a piece, so that another peer can download it. A claim that
// has since passed to a different peer is left alone: a peer releasing a piece it no longer holds must not take the
// claim away from the peer that has since taken it up.
func (t *tracker) clearDownload(index int, who *peer) {
	t.lock.Lock()
	if t.who[index] == who {
		delete(t.who, index)
		t.downloading.Unset(index)
	}
	t.lock.Unlock()
}

// bitField returns the bytes describing which pieces we have, or nil if we have none of them.
func (t *tracker) bitField() []byte {
	t.lock.RLock()
	defer t.lock.RUnlock()
	if !t.have.AnySet() {
		return nil
	}
	return t.have.Bytes()
}

// hasPiece returns true if the piece with the given index has been downloaded and verified.
func (t *tracker) hasPiece(index int) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.have.IsSet(index)
}

// isInteresting returns true if the peer these pieces belong to has anything we don't. A piece another peer has
// already claimed still counts: near the end of a download every piece we're missing can be claimed at once, and
// answering that we aren't interested then costs us exactly the connections we still need — typical remotes choke a
// peer that says it wants nothing, and worseForRotation ranks an uninterested peer first to drop — while getting one
// back once a claim frees up takes an interested/unchoke round trip, or a reconnect, every time.
func (t *tracker) isInteresting(has *fixedbits.Bits) bool {
	t.lock.RLock()
	i := fixedbits.FirstMissing(has, t.have)
	t.lock.RUnlock()
	return i != -1
}

// selectForDownloading claims a piece the peer has and we still need, returning its index, or -1 if there is nothing
// for it to take. Nothing is claimed once as many peers as ConcurrentDownloads allows are already downloading, which
// is what makes that option the limit on simultaneous downloads it says it is: without the cap here it only decided
// whether more peers were sought, while every peer that unchoked us went on to start a download of its own regardless.
func (t *tracker) selectForDownloading(who *peer, has *fixedbits.Bits) int {
	t.lock.Lock()
	defer t.lock.Unlock()
	if !t.downloadSlotAvailable(who) {
		return -1
	}
	i := fixedbits.FirstAvailable(has, t.downloading, t.have)
	if i != -1 {
		t.who[i] = who
		t.downloading.Set(i)
	}
	return i
}

// downloadSlotAvailable returns true if the peer may take on a piece, which it may either because it is already one of
// the peers downloading or because fewer than ConcurrentDownloads peers are. The claims are counted by the peers
// holding them rather than by the pieces claimed, since what the option limits is how many peers we download from.
// The lock must be held.
func (t *tracker) downloadSlotAvailable(who *peer) bool {
	downloaders := make(map[*peer]bool, len(t.who))
	for _, one := range t.who {
		if one == who {
			return true
		}
		downloaders[one] = true
	}
	return len(downloaders) < t.client.concurrentDownloads
}
