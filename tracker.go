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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/container/bits"
	"github.com/richardwilkes/torrent/tio"
	"github.com/zeebo/bencode"
)

const (
	startedMsg = "started"
	stoppedMsg = "stopped"
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
	lock sync.RWMutex
}

type trackerLockData struct {
	have            *bits.Bits
	downloading     *bits.Bits
	who             map[int]*peer
	peerAddresses   map[string]int
	seedExpires     time.Time
	trackerID       string
	currentState    State
	uploadedBytes   int64
	downloadedBytes int64
	totalBytes      int64
	remainingBytes  int64
	interval        int
	leechers        int
	seeders         int
	progress        float64
	started         bool
}

type trackerWire struct { //nolint:govet // We can't change the order of these fields
	Interval      int    `bencode:"interval"`
	PeerAddresses any    `bencode:"peers"`
	Seeders       int    `bencode:"complete"`
	Leechers      int    `bencode:"incomplete"`
	TrackerID     string `bencode:"tracker id"`
	Failure       string `bencode:"failure reason"`
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
			have:           bits.New(totalPieces),
			downloading:    bits.New(totalPieces),
			who:            make(map[int]*peer),
		},
	}
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
	if seedingTransition && t.client.downloadCompleteNotifier != nil {
		t.client.downloadCompleteNotifier <- t.client
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

func (t *tracker) periodicAnnounce() {
	for {
		t.lock.RLock()
		seconds := t.interval
		t.lock.RUnlock()
		if seconds < 300 { // No more often than every 5 minutes
			seconds = 300
		}
		timer := time.After(time.Duration(seconds) * time.Second)
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

func (t *tracker) announce(event string) error {
	slog.Debug("announce", "url", t.announceURL(event))
	in, err := t.get(t.announceURL(event))
	if err != nil {
		return err
	}
	if in.Failure != "" {
		return errs.New(in.Failure)
	}
	if in.Interval < 1 {
		return errs.New("invalid interval")
	}
	externalAddr := "<unknown>"
	if extIP := t.client.ExternalIP(); extIP != nil {
		externalAddr = extIP.String()
	}
	peerAddresses := make(map[string]int)
	switch value := in.PeerAddresses.(type) {
	case string:
		slog.Debug("announce string", "peers_list", value)
		for i := 0; i < len(value); i += 6 {
			addr := net.IPv4(value[i], value[i+1], value[i+2], value[i+3]).String()
			if addr != externalAddr {
				port := int(binary.BigEndian.Uint16([]byte(value[i+4 : i+6])))
				if port != 0 {
					peerAddresses[addr] = port
				}
			}
		}
	case []map[string]any:
		var inPeerAddresses []struct {
			ID   string `bencode:"peer id"`
			IP   string `bencode:"ip"`
			Port int    `bencode:"port"`
		}
		var data []byte
		if data, err = bencode.EncodeBytes(in.PeerAddresses); err != nil {
			return errs.Wrap(err)
		}
		if err = bencode.DecodeBytes(data, &inPeerAddresses); err != nil {
			return errs.Wrap(err)
		}
		slog.Debug("announce map", "count", len(inPeerAddresses))
		for _, one := range inPeerAddresses {
			if one.IP != externalAddr && one.Port != 0 {
				peerAddresses[one.IP] = one.Port
			}
		}
	default:
		slog.Debug("announce: unknown peer address format", "type", fmt.Sprintf("%T", in.PeerAddresses))
	}
	t.lock.Lock()
	t.interval = in.Interval
	t.trackerID = in.TrackerID
	t.seeders = in.Seeders
	t.leechers = in.Leechers
	t.peerAddresses = peerAddresses
	switch event {
	case startedMsg:
		t.started = true
	case stoppedMsg:
		t.started = false
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
	t.lock.RLock()
	fmt.Fprintf(&buffer, "&uploaded=%d", t.uploadedBytes)
	fmt.Fprintf(&buffer, "&downloaded=%d", t.downloadedBytes)
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
	var in trackerWire
	if err = bencode.NewDecoder(resp.Body).Decode(&in); err != nil {
		return nil, errs.Wrap(err)
	}
	return &in, nil
}

func (t *tracker) clearDownload(index int) {
	t.lock.Lock()
	delete(t.who, index)
	t.downloading.Unset(index)
	t.lock.Unlock()
}

func (t *tracker) isInteresting(has *bits.Bits) bool {
	t.lock.RLock()
	i := bits.FirstAvailable(has, t.downloading, t.have)
	t.lock.RUnlock()
	return i != -1
}

func (t *tracker) selectForDownloading(who *peer, has *bits.Bits) int {
	t.lock.Lock()
	i := bits.FirstAvailable(has, t.downloading, t.have)
	if i != -1 {
		t.who[i] = who
		t.downloading.Set(i)
	}
	t.lock.Unlock()
	return i
}
