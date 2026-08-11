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
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/zeebo/bencode"
)

// TestParseCompactPeers verifies that the compact peer list, which comes from a tracker and is therefore unverified
// data, is parsed without running off the end of a truncated or malformed list.
func TestParseCompactPeers(t *testing.T) {
	c := check.New(t)
	const unknownAddr = "<unknown>"
	list := string([]byte{
		10, 0, 0, 1, 0x1A, 0xE1, // 10.0.0.1:6881
		10, 0, 0, 2, 0x1A, 0xE2, // 10.0.0.2:6882
		10, 0, 0, 3, 0x1A, 0xE3, // 10.0.0.3:6883
	})

	peers := parseCompactPeers(list, unknownAddr)
	c.Equal(3, len(peers))
	c.Equal(6881, peers["10.0.0.1"])
	c.Equal(6882, peers["10.0.0.2"])
	c.Equal(6883, peers["10.0.0.3"])

	// A truncated final entry is discarded rather than read beyond the end of the list
	for i := 1; i < 6; i++ {
		truncated := list[:len(list)-i]
		c.NotPanics(func() { parseCompactPeers(truncated, unknownAddr) }, "truncated by %d", i)
		c.Equal(2, len(parseCompactPeers(truncated, unknownAddr)), "truncated by %d", i)
	}

	// A list too short to hold even one entry yields nothing
	for i := range 6 {
		c.NotPanics(func() { parseCompactPeers(list[:i], unknownAddr) }, "length %d", i)
		c.Equal(0, len(parseCompactPeers(list[:i], unknownAddr)), "length %d", i)
	}
	c.Equal(0, len(parseCompactPeers("", unknownAddr)))

	// Our own address is omitted, as are entries without a port
	c.Equal(2, len(parseCompactPeers(list, "10.0.0.2")))
	c.Equal(0, len(parseCompactPeers(string([]byte{10, 0, 0, 4, 0, 0}), unknownAddr)))
}

// TestParsePeersDictModel verifies that a tracker which ignores our request for the compact peer list and answers with
// the dict model instead is understood rather than having its peers silently discarded, which would leave us with no
// one to talk to.
func TestParsePeersDictModel(t *testing.T) {
	c := check.New(t)
	in := decodeTestTrackerResponse(t, []any{
		testPeerDict("aaaaaaaaaaaaaaaaaaaa", "10.0.0.1", 6881),
		testPeerDict("", "10.0.0.2", 6882),
		testPeerDict("", "10.0.0.3", 0),    // No port, so it is dropped
		testPeerDict("", "10.0.0.9", 6889), // Our own address, so it is dropped
	})
	peers, err := parsePeers(in.PeerAddresses, "10.0.0.9")
	c.NoError(err)
	c.Equal(2, len(peers))
	c.Equal(6881, peers["10.0.0.1"])
	c.Equal(6882, peers["10.0.0.2"])

	// An empty list is legal and simply yields no peers
	in = decodeTestTrackerResponse(t, []any{})
	peers, err = parsePeers(in.PeerAddresses, "10.0.0.9")
	c.NoError(err)
	c.Equal(0, len(peers))
}

// TestParsePeersCompact verifies that the compact form still travels the same path as the dict model.
func TestParsePeersCompact(t *testing.T) {
	c := check.New(t)
	in := decodeTestTrackerResponse(t, string([]byte{
		10, 0, 0, 1, 0x1A, 0xE1, // 10.0.0.1:6881
		10, 0, 0, 2, 0x1A, 0xE2, // 10.0.0.2:6882
	}))
	peers, err := parsePeers(in.PeerAddresses, "<unknown>")
	c.NoError(err)
	c.Equal(2, len(peers))
	c.Equal(6881, peers["10.0.0.1"])
	c.Equal(6882, peers["10.0.0.2"])
}

// TestParsePeersMalformed verifies that a response without a usable peer list is handled rather than misread. A
// tracker's response is unverified data, so anything at all may show up in it.
func TestParsePeersMalformed(t *testing.T) {
	c := check.New(t)

	// A response with no "peers" key at all leaves nothing to parse
	var missing trackerWire
	c.NoError(bencode.DecodeBytes([]byte("d8:intervali1800ee"), &missing))
	peers, err := parsePeers(missing.PeerAddresses, "<unknown>")
	c.NoError(err)
	c.Equal(0, len(peers))

	// Neither a string nor a list, so there is nothing sensible to make of it
	_, err = parsePeers(decodeTestTrackerResponse(t, 5).PeerAddresses, "<unknown>")
	c.HasError(err)

	// A list holding something other than peer dictionaries
	_, err = parsePeers(decodeTestTrackerResponse(t, []any{"10.0.0.1"}).PeerAddresses, "<unknown>")
	c.HasError(err)
}

// noPeersResponse is a tracker response carrying an interval and an empty peer list, which is everything an announce
// needs and nothing more.
const noPeersResponse = "d8:intervali1800e5:peers0:e"

// overflowSeconds is the smallest interval, in seconds, whose conversion to a time.Duration overflows. The arithmetic
// is done in int64 and only then narrowed, since letting math.MaxInt64 become an int is a constant that overflows the
// type on a 32-bit platform, where this file would no longer compile at all. The clamp is for the same platform, where
// no int is large enough to overflow a Duration in the first place, but where the largest one still has to be bounded
// to the maximum announce interval.
const overflowSeconds = int(min(int64(math.MaxInt), math.MaxInt64/int64(time.Second)+1))

// TestAnnounceIntervalIsBounded verifies the wait between announces stays within sane limits no matter what the
// tracker asks for. A value large enough to overflow the conversion to a time.Duration yields a negative delay, whose
// timer fires immediately and turns the periodic announce into a tight loop of HTTP round trips.
func TestAnnounceIntervalIsBounded(t *testing.T) {
	for _, one := range []struct {
		name     string
		seconds  int
		expected time.Duration
	}{
		{name: "unset", seconds: 0, expected: minAnnounceInterval},
		{name: "negative", seconds: -1, expected: minAnnounceInterval},
		{name: "hugely negative", seconds: math.MinInt, expected: minAnnounceInterval},
		{name: "too frequent", seconds: 60, expected: minAnnounceInterval},
		{name: "at the minimum", seconds: int(minAnnounceInterval / time.Second), expected: minAnnounceInterval},
		{name: "reasonable", seconds: 1800, expected: 30 * time.Minute},
		{name: "at the maximum", seconds: int(maxAnnounceInterval / time.Second), expected: maxAnnounceInterval},
		{name: "beyond the maximum", seconds: 30 * 24 * 60 * 60, expected: maxAnnounceInterval},
		{name: "overflows a duration", seconds: math.MaxInt, expected: maxAnnounceInterval},
		{name: "just past the overflow point", seconds: overflowSeconds, expected: maxAnnounceInterval},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			var tr tracker
			tr.interval = one.seconds
			actual := tr.announceInterval()
			c.Equal(one.expected, actual)
			c.True(actual >= minAnnounceInterval, "an interval of %d yielded %v", one.seconds, actual)
		})
	}
}

// TestCheckBencode verifies the structural check applied to a tracker's response before the decoder sees it. The
// decoder allocates a string's declared length before reading it, so a length that isn't backed by actual data has to
// be caught here.
func TestCheckBencode(t *testing.T) {
	c := check.New(t)
	for _, one := range []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "empty dict", data: "de"},
		{name: "typical response", data: "d8:intervali1800e5:peers12:" + strings.Repeat("x", 12) + "e"},
		{name: "dict model peers", data: "d5:peersld2:ip8:10.0.0.14:porti6881eeee"},
		{name: "negative integer", data: "d8:intervali-1ee"},
		{name: "empty string", data: "d9:trackerid0:e"},
	} {
		c.NoError(checkBencode([]byte(one.data)), one.name)
	}

	for _, one := range []struct {
		name string
		data string
	}{
		{name: "string length with no data behind it", data: "d5:peers2147483646:"},
		{name: "string one byte longer than the data", data: "d5:peers4:abe"},
		{name: "string length that isn't a number", data: "d5:peers99999999999999999999:abe"},
		{name: "unterminated string length", data: "d5:peers12"},
		{name: "unterminated integer", data: "d8:intervali1800"},
		{name: "unmatched end marker", data: "dee"},
		{name: "truncated dict", data: "d8:intervali1800e"},
		{name: "unexpected byte", data: "d8:intervalx1800ee"},
		{name: "nested too deeply", data: strings.Repeat("l", maxBencodeDepth+1) + strings.Repeat("e", maxBencodeDepth+1)},
	} {
		c.HasError(checkBencode([]byte(one.data)), one.name)
	}

	// Nesting right up to the limit is still accepted
	c.NoError(checkBencode([]byte(strings.Repeat("l", maxBencodeDepth) + strings.Repeat("e", maxBencodeDepth))))
}

// TestTrackerResponseIsBounded verifies that a tracker, which is an untrusted source, can neither hand us an
// unbounded response nor talk us into a huge allocation with a few bytes.
func TestTrackerResponseIsBounded(t *testing.T) {
	c := check.New(t)
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	var tr tracker

	// A response declaring a 2GB string, with nothing behind it, must not be allocated for
	body = "d5:peers2147483646:"
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := tr.get(srv.URL)
	runtime.ReadMemStats(&after)
	c.HasError(err)
	const allowed = 64 * 1024 * 1024
	c.True(after.TotalAlloc-before.TotalAlloc < allowed, "allocated %d bytes for a %d byte response",
		after.TotalAlloc-before.TotalAlloc, len(body))

	// A response larger than the cap is refused rather than read into memory
	body = "d5:peers" + strconv.Itoa(2*maxTrackerResponseSize) + ":" + strings.Repeat("x", 2*maxTrackerResponseSize) + "e"
	_, err = tr.get(srv.URL)
	c.HasError(err)

	// A normal response still decodes
	body = "d8:intervali1800e8:completei2e10:incompletei1e5:peers6:" + string([]byte{10, 0, 0, 1, 0x1A, 0xE1}) + "e"
	in, err := tr.get(srv.URL)
	c.NoError(err)
	c.Equal(1800, in.Interval)
	c.Equal(2, in.Seeders)
	c.Equal(1, in.Leechers)
	peers, err := parsePeers(in.PeerAddresses, "<unknown>")
	c.NoError(err)
	c.Equal(6881, peers["10.0.0.1"])
}

// TestAnnounceReportsTransferTotals verifies that the bytes moved on this torrent's behalf reach the tracker. Every
// announce reporting zero misreports the transfer and breaks ratio accounting on trackers that use the values.
func TestAnnounceReportsTransferTotals(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)

	c.Contains(client.tracker.announceURL(startedMsg), "&uploaded=0&downloaded=0")
	client.tracker.addUploadedBytes(4096)
	client.tracker.addDownloadedBytes(16397)
	client.tracker.addDownloadedBytes(3)
	c.Contains(client.tracker.announceURL(""), "&uploaded=4096&downloaded=16400")
}

// TestAnnounceToleratesTheShutdownResponse verifies that a tracker answering the stopped announce minimally doesn't
// produce a spurious error, and that we stop considering ourselves started either way. The response to that announce
// is never used — the stop was delivered by the request itself.
func TestAnnounceToleratesTheShutdownResponse(t *testing.T) {
	for _, one := range []struct {
		name string
		body string
	}{
		{name: "empty dict", body: "de"},
		{name: "no interval", body: "d8:completei2ee"},
		{name: "zero interval", body: "d8:intervali0ee"},
		{name: "failure reason", body: "d14:failure reason9:not founde"},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			d := newTestDispatcher(t)
			client, body := newTestTrackerClient(t, d)
			*body = one.body
			client.tracker.lock.Lock()
			client.tracker.started = true
			client.tracker.lock.Unlock()

			c.NoError(client.tracker.announce(stoppedMsg))
			c.False(client.tracker.hasStarted(), "the stop was delivered, so we are no longer started")

			// The same response is still not enough to start with, since the periodic announce needs an interval
			c.HasError(client.tracker.announce(startedMsg))
			c.False(client.tracker.hasStarted())
		})
	}
}

// TestAnnounceKeepsTheIntervalWhenOneIsNotReturned verifies that an update or completion announce answered without an
// interval isn't treated as a failure and doesn't disturb the interval already in hand.
func TestAnnounceKeepsTheIntervalWhenOneIsNotReturned(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, body := newTestTrackerClient(t, d)
	*body = noPeersResponse

	c.NoError(client.tracker.announce(startedMsg))
	c.True(client.tracker.hasStarted())
	c.Equal(30*time.Minute, client.tracker.announceInterval())

	*body = "d5:peers0:e"
	c.NoError(client.tracker.announce("completed"))
	c.Equal(30*time.Minute, client.tracker.announceInterval())

	// A failure reason is still an error for anything but the shutdown announce
	*body = "d14:failure reason9:not founde"
	c.HasError(client.tracker.announce(""))
}

// TestAnnounceKeepsTheTrackerID verifies that a tracker id we were issued is sent back on every announce that
// follows, including the ones answered without one. A tracker that issues an id with the start response and leaves it
// out of the rest — which BEP 3 allows, and says we must go on returning the id we were given — would otherwise stop
// being told which session we are as soon as the next announce went out.
func TestAnnounceKeepsTheTrackerID(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, body := newTestTrackerClient(t, d)

	*body = "d8:intervali1800e5:peers0:10:tracker id5:abcdee"
	c.NoError(client.tracker.announce(startedMsg))
	c.Contains(client.tracker.announceURL(""), "&trackerid=abcde")

	// A response that simply leaves the key out isn't taking the id away
	*body = noPeersResponse
	c.NoError(client.tracker.announce(""))
	c.Contains(client.tracker.announceURL(""), "&trackerid=abcde")

	// A new id does replace it, and is escaped for the query it goes into
	*body = "d8:intervali1800e5:peers0:10:tracker id5:a b&ce"
	c.NoError(client.tracker.announce(""))
	c.Contains(client.tracker.announceURL(""), "&trackerid=a+b%26c")

	// A tracker that never issues one leaves the parameter out entirely
	client, body = newTestTrackerClient(t, d)
	*body = noPeersResponse
	c.NoError(client.tracker.announce(startedMsg))
	c.NotContains(client.tracker.announceURL(""), "trackerid")
}

// TestStopAnnounceDoesNotWaitForThePeriodicAnnounce verifies that stopping doesn't have to wait for an announce that
// is already under way. The periodic announce goroutine spends up to the 30 second HTTP timeout inside a single
// request, and the stop being handed to it directly would hold up the stopped announce, the close of the storage file
// and the stopped notification for all of that time, well beyond the timeout the caller gave Stop.
func TestStopAnnounceDoesNotWaitForThePeriodicAnnounce(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, body := newTestTrackerClient(t, d)
	*body = noPeersResponse
	c.NoError(client.tracker.announce(startedMsg))

	// Nothing is listening for the stop, which is exactly the state the periodic announce is in for as long as one of
	// its own requests is outstanding
	stopped := make(chan error, 1)
	go func() { stopped <- client.tracker.announceStopped() }()
	select {
	case err := <-stopped:
		c.NoError(err)
	case <-time.After(peerMgmtWait):
		t.Fatal("the stopped announce waited on the periodic announce")
	}
	c.False(client.tracker.hasStarted())

	// The stop is still there to be found by a periodic announce that only looks afterwards, rather than having been
	// consumed by whoever happened to be listening first
	returned := make(chan struct{})
	go func() { defer close(returned); client.tracker.periodicAnnounce() }()
	select {
	case <-returned:
	case <-time.After(peerMgmtWait):
		t.Fatal("the periodic announce never saw the stop")
	}
}

// newTestTrackerClient returns a client whose announces go to a stub tracker, along with a pointer to the response
// body that stub will return. The peer ID is filled in with query-safe bytes, which is what NewClient does and what
// the announce URL relies on.
func newTestTrackerClient(t *testing.T, d *dispatcher.Dispatcher) (client *Client, body *string) {
	t.Helper()
	body = new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, *body)
	}))
	t.Cleanup(srv.Close)
	client = newTestClient(d)
	client.torrentFile.Announce = srv.URL
	for i := range client.id {
		client.id[i] = urlQuerySafeBytes[i%len(urlQuerySafeBytes)]
	}
	return client, body
}

// testPeerDict builds one entry of a tracker's dict-model peer list. The keys are spelled out the way a tracker would
// send them, rather than being taken from the production struct's tags, so that a change to those tags is caught. The
// peer ID is optional, since trackers asked for the compact list often leave it out of the dict model as well.
func testPeerDict(id, ip string, port int) map[string]any {
	one := map[string]any{"ip": ip, "port": port}
	if id != "" {
		one["peer id"] = id
	}
	return one
}

// decodeTestTrackerResponse builds a tracker response with the given "peers" value and decodes it the same way an
// actual response would be, so that the raw bencode reaching parsePeers is what a tracker would have sent.
func decodeTestTrackerResponse(t *testing.T, peers any) *trackerWire {
	t.Helper()
	data, err := bencode.EncodeBytes(map[string]any{
		"interval":   1800,
		"complete":   2,
		"incomplete": 1,
		"peers":      peers,
	})
	if err != nil {
		t.Fatal(err)
	}
	var in trackerWire
	if err = bencode.DecodeBytes(data, &in); err != nil {
		t.Fatal(err)
	}
	return &in
}
