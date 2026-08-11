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
	"net/url"
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

	// stopAnnounceWait is how long the shutdown will wait for the periodic announce goroutine to finish. The request
	// that goroutine may be in the middle of is aborted before the wait begins, so this is only reached if the
	// transport doesn't unwind from that promptly; the shutdown then goes ahead rather than being held up further.
	stopAnnounceWait = 5 * time.Second
)

var (
	// TrackerUserAgent will be used as the http client user agent header if not empty.
	TrackerUserAgent = ""
	httpClient       = &http.Client{Timeout: 30 * time.Second}
)

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
	have          *fixedbits.Bits
	downloading   *fixedbits.Bits
	who           map[int]*peer
	peerAddresses map[string]int
	// periodicAnnounceDone is closed by the periodic announce goroutine as it returns, which is what the shutdown
	// waits on. It stays nil while no such goroutine exists, so a tracker whose start announce failed has nothing to
	// wait for.
	periodicAnnounceDone chan struct{}
	seedExpires          time.Time
	trackerID            string
	currentState         State
	totalBytes           int64
	remainingBytes       int64
	interval             int
	leechers             int
	seeders              int
	progress             float64
	started              bool
}

type trackerWire struct { //nolint:govet // We can't change the order of these fields
	Interval int `bencode:"interval"`
	// PeerAddresses is left as raw bencode because a tracker may answer with either the compact form (a string) or the
	// dict model (a list of dictionaries), regardless of our request for the compact form.
	PeerAddresses bencode.RawMessage `bencode:"peers"`
	Seeders       int                `bencode:"complete"`
	Leechers      int                `bencode:"incomplete"`
	TrackerID     string             `bencode:"tracker id"`
	Failure       string             `bencode:"failure reason"`
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

func (t *tracker) markBlockValid(index int) {
	announce := false
	t.lock.Lock()
	if !t.have.IsSet(index) {
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
	t.client.informPeersWeHavePiece(index)
	if announce {
		t.requestCompleteAnnounce()
	}
}

// requestCompleteAnnounce hands the "completed" announce to the periodic announce goroutine rather than making it
// here. This runs on a peer's message loop, which the client's peer wait group tracks, so making the announce here
// stalls that peer for a full tracker round trip and, when a stop lands while it is in flight, holds up the shutdown
// — the stopped announce, the close of the storage file and the stopped notification all queue behind the wait for
// that peer — for as long as the HTTP client's timeout allows, well past the bound the shutdown otherwise keeps every
// announce within. A download that finished before we ever announced a start, which is what verifying an already
// complete file on disk does, has no completion to report and no goroutine to report it: the event describes a
// torrent downloaded during this session.
func (t *tracker) requestCompleteAnnounce() {
	if !t.hasStarted() {
		return
	}
	select {
	case t.completeAnnounceRequested <- struct{}{}:
	default:
	}
}

func (t *tracker) peerAddressesMap() map[string]int {
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

func (t *tracker) announceStart() error {
	if t.hasStarted() {
		return nil
	}
	if err := t.announce(context.Background(), startedMsg); err != nil {
		return err
	}
	if t.isDownloadComplete() {
		t.setState(Seeding)
	} else {
		t.setStateAndProgress(Downloading, -1)
	}
	t.startPeriodicAnnounce()
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
// small enough not to overflow it.
func (t *tracker) announceInterval() time.Duration {
	t.lock.RLock()
	seconds := t.interval
	t.lock.RUnlock()
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

// parseCompactPeers extracts the peer addresses from the compact peer list format, which is a series of 6-byte
// entries, each holding a 4-byte IPv4 address followed by a 2-byte port. A tracker response is unverified data, so a
// trailing partial entry is ignored rather than allowed to run off the end of the list. Our own address, which the
// caller passes in, is omitted, as are entries with no port.
func parseCompactPeers(value, externalAddr string) map[string]int {
	peerAddresses := make(map[string]int, len(value)/6)
	for i := 0; i+6 <= len(value); i += 6 {
		addr := net.IPv4(value[i], value[i+1], value[i+2], value[i+3]).String()
		if addr != externalAddr {
			if port := int(binary.BigEndian.Uint16([]byte(value[i+4 : i+6]))); port != 0 {
				peerAddresses[addr] = port
			}
		}
	}
	return peerAddresses
}

// parsePeers extracts the peer addresses from a tracker's "peers" value. Although we always ask for the compact form,
// a tracker is free to ignore that and answer with the dict model instead, so both are accepted: a bencoded string is
// the compact form and a bencoded list is the dict model. A missing or empty value simply yields no peers. Our own
// address, which the caller passes in, is omitted, as are entries whose port isn't one that can be dialed.
func parsePeers(raw bencode.RawMessage, externalAddr string) (map[string]int, error) {
	if len(raw) == 0 {
		return make(map[string]int), nil
	}
	switch {
	case raw[0] >= '0' && raw[0] <= '9': // A bencoded string, so the compact form
		var compact string
		if err := bencode.DecodeBytes(raw, &compact); err != nil {
			return nil, errs.NewWithCause("unable to decode compact peer list", err)
		}
		slog.Debug("announce string", "peers_list", compact)
		return parseCompactPeers(compact, externalAddr), nil
	case raw[0] == 'l': // A bencoded list, so the dict model
		var list []peerWire
		if err := bencode.DecodeBytes(raw, &list); err != nil {
			return nil, errs.NewWithCause("unable to decode peer list", err)
		}
		slog.Debug("announce map", "count", len(list))
		peerAddresses := make(map[string]int, len(list))
		for _, one := range list {
			// The dict model carries the port as a bencoded integer, so unlike the compact form — whose ports are
			// inherently 16 bit — a hostile or buggy tracker can hand us a negative one or one past the end of the port
			// range. Kept, they become dial attempts that cannot possibly succeed, retried on every peer management
			// pass until the next announce replaces the list.
			if one.IP != externalAddr && one.Port > 0 && one.Port <= math.MaxUint16 {
				peerAddresses[one.IP] = one.Port
			}
		}
		return peerAddresses, nil
	default:
		return nil, errs.Newf("unknown peer address format: %q", raw[0])
	}
}

func (t *tracker) announce(ctx context.Context, event string) error {
	urlStr := t.announceURL(event)
	slog.Debug("announce", "url", urlStr)
	resp, err := t.request(ctx, urlStr)
	if err != nil {
		return err
	}
	defer xio.DiscardAndCloseIgnoringErrors(resp.Body)
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
	peerAddresses, err := parsePeers(in.PeerAddresses, externalAddr)
	if err != nil {
		return err
	}
	// An announce the shutdown overtook has nothing left to say. What it came back with describes a swarm we are on
	// our way out of, and letting the peer list, the interval and the counts we report land after the stopped event
	// would leave all of them belonging to a client that no longer exists. The start announce is exempt: it is what
	// decides whether a stopped event is owed at all, and it completes before any stop can be signaled.
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
	t.peerAddresses = peerAddresses
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
	buffer.WriteString(url.QueryEscape(string(t.client.torrentFile.InfoHash[:])))
	buffer.WriteString("&peer_id=")
	buffer.Write(t.client.id[:])
	fmt.Fprintf(&buffer, "&port=%d", t.client.dispatcher.ExternalPort())
	fmt.Fprintf(&buffer, "&uploaded=%d", t.uploadedBytes.Load())
	fmt.Fprintf(&buffer, "&downloaded=%d", t.downloadedBytes.Load())
	t.lock.RLock()
	fmt.Fprintf(&buffer, "&left=%d", t.remainingBytes)
	if t.trackerID != "" {
		fmt.Fprintf(&buffer, "&trackerid=%s", url.QueryEscape(t.trackerID))
	}
	t.lock.RUnlock()
	fmt.Fprintf(&buffer, "&numwant=%d", t.client.peersWanted)
	buffer.WriteString("&compact=1")
	if event != "" {
		fmt.Fprintf(&buffer, "&event=%s", event)
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
	if TrackerUserAgent != "" {
		req.Header.Set("user-agent", TrackerUserAgent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return resp, nil
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
