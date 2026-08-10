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
	startedMsg = "started"
	stoppedMsg = "stopped"

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
)

var (
	// TrackerUserAgent will be used as the http client user agent header if not empty.
	TrackerUserAgent = ""
	httpClient       = &http.Client{Timeout: 30 * time.Second}
)

type tracker struct {
	client           *Client
	stopAnnounceChan chan bool
	trackerLockData
	// The transfer totals are reported in every announce and are updated by each peer's read and write goroutines, so
	// they are kept as atomics rather than under the tracker lock, which those hot paths would otherwise contend on
	// for every message.
	uploadedBytes   atomic.Int64
	downloadedBytes atomic.Int64
	lock            sync.RWMutex
}

type trackerLockData struct {
	have           *fixedbits.Bits
	downloading    *fixedbits.Bits
	who            map[int]*peer
	peerAddresses  map[string]int
	seedExpires    time.Time
	trackerID      string
	currentState   State
	totalBytes     int64
	remainingBytes int64
	interval       int
	leechers       int
	seeders        int
	progress       float64
	started        bool
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
	return &tracker{
		client:           client,
		stopAnnounceChan: make(chan bool),
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
		if err := t.announceComplete(); err != nil {
			errs.LogTo(t.client.logger, err)
		}
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
	if err := t.announce(startedMsg); err != nil {
		return err
	}
	if t.isDownloadComplete() {
		t.setState(Seeding)
	} else {
		t.setStateAndProgress(Downloading, -1)
	}
	go t.periodicAnnounce()
	return nil
}

func (t *tracker) announceComplete() error {
	if !t.hasStarted() {
		return nil
	}
	return t.announce("completed")
}

func (t *tracker) announceStopped() error {
	if !t.hasStarted() {
		return nil
	}
	t.stopAnnounceChan <- true
	return t.announce(stoppedMsg)
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

func (t *tracker) periodicAnnounce() {
	for {
		timer := time.After(t.announceInterval())
		select {
		case <-timer:
			if err := t.announce(""); tio.ShouldLogIOError(err) {
				errs.LogTo(t.client.logger, err)
			}
		case <-t.stopAnnounceChan:
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
// address, which the caller passes in, is omitted, as are entries with no port.
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
			if one.IP != externalAddr && one.Port != 0 {
				peerAddresses[one.IP] = one.Port
			}
		}
		return peerAddresses, nil
	default:
		return nil, errs.Newf("unknown peer address format: %q", raw[0])
	}
}

func (t *tracker) announce(event string) error {
	slog.Debug("announce", "url", t.announceURL(event))
	in, err := t.get(t.announceURL(event))
	if err != nil {
		return err
	}
	if event == stoppedMsg {
		// Nothing in the response to a shutdown announce is ever used: the stop was delivered by the request itself,
		// which has already succeeded. Holding it to the same standard as the rest only produces a spurious error and
		// leaves us believing we are still started.
		t.lock.Lock()
		t.started = false
		t.lock.Unlock()
		t.client.logger.Info("announce", "event", stoppedMsg)
		return nil
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
	t.lock.Lock()
	if in.Interval > 0 {
		t.interval = in.Interval
	}
	t.trackerID = in.TrackerID
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

func (t *tracker) get(urlStr string) (*trackerWire, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	defer xio.DiscardAndCloseIgnoringErrors(resp.Body)
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

func (t *tracker) isInteresting(has *fixedbits.Bits) bool {
	t.lock.RLock()
	i := fixedbits.FirstAvailable(has, t.downloading, t.have)
	t.lock.RUnlock()
	return i != -1
}

func (t *tracker) selectForDownloading(who *peer, has *fixedbits.Bits) int {
	t.lock.Lock()
	i := fixedbits.FirstAvailable(has, t.downloading, t.have)
	if i != -1 {
		t.who[i] = who
		t.downloading.Set(i)
	}
	t.lock.Unlock()
	return i
}
