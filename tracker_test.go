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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/torrent/dispatcher"
	"github.com/zeebo/bencode"
)

// TestParseCompactPeers verifies that the compact peer list, which comes from a tracker and is therefore unverified
// data, is parsed without running off the end of a truncated or malformed list.
func TestParseCompactPeers(t *testing.T) {
	c := check.New(t)
	const unknownAddr = "<unknown>"
	const unknownPort = 0
	list := string([]byte{
		10, 0, 0, 1, 0x1A, 0xE1, // 10.0.0.1:6881
		10, 0, 0, 2, 0x1A, 0xE2, // 10.0.0.2:6882
		10, 0, 0, 3, 0x1A, 0xE3, // 10.0.0.3:6883
	})

	peers := parseCompactPeers(list, unknownAddr, unknownPort, testPeerLimit)
	c.Equal([]peerAddr{
		{ip: testPeerIP1, port: 6881},
		{ip: testPeerIP2, port: 6882},
		{ip: testPeerIP3, port: 6883},
	}, peers)

	// A truncated final entry is discarded rather than read beyond the end of the list
	for i := 1; i < 6; i++ {
		truncated := list[:len(list)-i]
		c.NotPanics(func() { parseCompactPeers(truncated, unknownAddr, unknownPort, testPeerLimit) }, "truncated by %d", i)
		c.Equal(2, len(parseCompactPeers(truncated, unknownAddr, unknownPort, testPeerLimit)), "truncated by %d", i)
	}

	// A list too short to hold even one entry yields nothing
	for i := range 6 {
		c.NotPanics(func() { parseCompactPeers(list[:i], unknownAddr, unknownPort, testPeerLimit) }, "length %d", i)
		c.Equal(0, len(parseCompactPeers(list[:i], unknownAddr, unknownPort, testPeerLimit)), "length %d", i)
	}
	c.Equal(0, len(parseCompactPeers("", unknownAddr, unknownPort, testPeerLimit)))

	// Only the entry that is actually us is omitted, not every peer sharing our IP, and entries without a port go too
	c.Equal([]peerAddr{
		{ip: testPeerIP1, port: 6881},
		{ip: testPeerIP3, port: 6883},
	}, parseCompactPeers(list, testPeerIP2, 6882, testPeerLimit))
	c.Equal(3, len(parseCompactPeers(list, testPeerIP2, 6889, testPeerLimit)),
		"a peer sharing our address on a port of its own is a NAT-mate, not us")
	c.Equal(0, len(parseCompactPeers(string([]byte{10, 0, 0, 4, 0, 0}), unknownAddr, unknownPort, testPeerLimit)))
}

// TestParsePeersKeepsPeersSharingAnAddress verifies that the peers of a swarm that sit behind one NAT, and so share
// one IP, all survive the parse. Keyed by IP alone, every one of them but the last is lost — and in a swarm that is
// mostly one NAT that is most of the peers we were given.
func TestParsePeersKeepsPeersSharingAnAddress(t *testing.T) {
	c := check.New(t)
	behindOneNAT := string([]byte{
		10, 0, 0, 1, 0x1A, 0xE1, // 10.0.0.1:6881
		10, 0, 0, 1, 0x1A, 0xE2, // 10.0.0.1:6882
		10, 0, 0, 1, 0x1A, 0xE3, // 10.0.0.1:6883
	})
	c.Equal([]peerAddr{
		{ip: testPeerIP1, port: 6881},
		{ip: testPeerIP1, port: 6882},
		{ip: testPeerIP1, port: 6883},
	}, parseCompactPeers(behindOneNAT, "<unknown>", 0, testPeerLimit))

	// The dict model says the same thing the long way around
	in := decodeTestTrackerResponse(t, []any{
		testPeerDict("", testPeerIP1, 6881),
		testPeerDict("", testPeerIP1, 6882),
		testPeerDict("", testPeerIP1, 6883),
	})
	peers, err := parsePeers(discardLogger(), in.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal(3, len(peers), "peers behind one NAT must not collapse into a single entry")
}

// TestParsePeersDictModel verifies that a tracker which ignores our request for the compact peer list and answers with
// the dict model instead is understood rather than having its peers silently discarded, which would leave us with no
// one to talk to.
func TestParsePeersDictModel(t *testing.T) {
	c := check.New(t)
	in := decodeTestTrackerResponse(t, []any{
		testPeerDict("aaaaaaaaaaaaaaaaaaaa", testPeerIP1, 6881),
		testPeerDict("", testPeerIP2, 6882),
		testPeerDict("", testPeerIP3, 0),   // No port, so it is dropped
		testPeerDict("", "10.0.0.9", 6889), // Our own address and port, so it is dropped
	})
	peers, err := parsePeers(discardLogger(), in.PeerAddresses, "10.0.0.9", 6889, testPeerLimit)
	c.NoError(err)
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}, {ip: testPeerIP2, port: 6882}}, peers)

	// An empty list is legal and simply yields no peers
	in = decodeTestTrackerResponse(t, []any{})
	peers, err = parsePeers(discardLogger(), in.PeerAddresses, "10.0.0.9", 6889, testPeerLimit)
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
	peers, err := parsePeers(discardLogger(), in.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}, {ip: testPeerIP2, port: 6882}}, peers)
}

// TestParsePeersDropsEntriesNamingNoHost verifies that a peer list entry which names no host in particular is refused
// rather than dialed. A tracker's peer list is unverified data, and both of the ways an entry can leave the host out
// point the dial back at the machine we're running on: net.JoinHostPort("", "6881") is ":6881" and the unspecified
// address is "0.0.0.0:6881", so a hostile tracker handing one back has us open connections and send BitTorrent
// handshakes to whatever local service is listening on the port it chose rather than to a peer.
func TestParsePeersDropsEntriesNamingNoHost(t *testing.T) {
	c := check.New(t)

	// The dict model takes the host verbatim from the tracker, so every one of these can arrive
	in := decodeTestTrackerResponse(t, []any{
		testPeerDict("", "", 6881),          // No host at all
		testPeerDict("", "0.0.0.0", 6882),   // The unspecified IPv4 address
		testPeerDict("", "::", 6883),        // The unspecified IPv6 address
		testPeerDict("", testPeerIP1, 6884), // A peer we could actually reach
	})
	peers, err := parsePeers(discardLogger(), in.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6884}}, peers)

	// The compact form fills the host in itself, but it can still spell out the unspecified address
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}}, parseCompactPeers(string([]byte{
		0, 0, 0, 0, 0x1A, 0xE2, // 0.0.0.0:6882
		10, 0, 0, 1, 0x1A, 0xE1, // 10.0.0.1:6881
	}), "<unknown>", 0, testPeerLimit))

	// A host that isn't an address at all is left alone, since BEP 3 lets the dict model carry a DNS name and
	// resolving it is the dial's business
	in = decodeTestTrackerResponse(t, []any{testPeerDict("", "peer.example.com", 6881)})
	peers, err = parsePeers(discardLogger(), in.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal([]peerAddr{{ip: "peer.example.com", port: 6881}}, peers)
}

// TestParsePeersMalformed verifies that a response without a usable peer list is handled rather than misread. A
// tracker's response is unverified data, so anything at all may show up in it.
func TestParsePeersMalformed(t *testing.T) {
	c := check.New(t)

	// A response with no "peers" key at all leaves nothing to parse
	var missing trackerWire
	c.NoError(bencode.DecodeBytes([]byte("d8:intervali1800ee"), &missing))
	peers, err := parsePeers(discardLogger(), missing.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal(0, len(peers))

	// Neither a string nor a list, so there is nothing sensible to make of it
	_, err = parsePeers(discardLogger(), decodeTestTrackerResponse(t, 5).PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.HasError(err)

	// A list holding something other than peer dictionaries
	_, err = parsePeers(discardLogger(), decodeTestTrackerResponse(t, []any{testPeerIP1}).PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.HasError(err)
}

const (
	// noPeersResponse is a tracker response carrying an interval and an empty peer list, which is everything an
	// announce needs and nothing more.
	noPeersResponse = "d8:intervali1800e5:peers0:e"

	// oneCompactPeerResponse is a tracker response carrying an interval and a compact peer list holding testPeerIP1 on
	// port 6881, for the tests that need to see the peer list a response came back with actually applied.
	oneCompactPeerResponse = "d8:intervali1800e5:peers6:\x0a\x00\x00\x01\x1a\xe1e"

	// unbackedStringResponse declares a 2GB string with nothing at all behind it, which is what a tracker hands us when
	// it wants us to allocate 2GB.
	unbackedStringResponse = "d5:peers2147483646:"

	// failureResponse is a tracker refusing the announce, the one thing in a response that is an error in its own
	// right rather than something missing from it.
	failureResponse = "d14:failure reason9:not founde"

	// testPeerIP1, testPeerIP2 and testPeerIP3 stand in for the addresses a tracker hands back in its peer list.
	testPeerIP1 = "10.0.0.1"
	testPeerIP2 = "10.0.0.2"
	testPeerIP3 = "10.0.0.3"

	// testUserAgent stands in for whatever an application puts in the user agent header of its announces.
	testUserAgent = "torrent-test-agent"

	// testPeerLimit is the cap the peer parsing tests pass when the cap isn't what they are judging: far above
	// anything they hand over, so that every entry they supply comes back.
	testPeerLimit = 1024

	// concurrentAgentSetters is how many goroutines change the user agent while announces are being made, each of them
	// paired with an announce of its own.
	concurrentAgentSetters = 4
)

// overflowSeconds is the smallest interval, in seconds, whose conversion to a time.Duration overflows. The arithmetic
// is done in int64 and only then narrowed, since letting math.MaxInt64 become an int is a constant that overflows the
// type on a 32-bit platform, where this file would no longer compile at all. The clamp is for the same platform, where
// no int is large enough to overflow a Duration in the first place, but where the largest one still has to be bounded
// to the maximum announce interval.
const overflowSeconds = int(min(int64(math.MaxInt), math.MaxInt64/int64(time.Second)+1))

// wrapSeconds is the negative interval closest to zero whose conversion to a time.Duration overflows the other way,
// landing back in the positive range at roughly 292 years. The clamp is again for a 32-bit platform, where no int
// reaches far enough to wrap and the most negative one stands in for it.
const wrapSeconds = int(max(int64(math.MinInt), -(math.MaxInt64/int64(time.Second) + 1)))

// TestAnnounceIntervalIsBounded verifies the wait between announces stays within sane limits no matter what the
// tracker asks for. A value large enough to overflow the conversion to a time.Duration yields a negative delay, whose
// timer fires immediately and turns the periodic announce into a tight loop of HTTP round trips, while one negative
// enough to overflow it the other way lands back in the positive range at a delay measured in centuries, which
// silences the periodic announce entirely.
func TestAnnounceIntervalIsBounded(t *testing.T) {
	for _, one := range []struct {
		name     string
		seconds  int
		expected time.Duration
	}{
		{name: "unset", seconds: 0, expected: minAnnounceInterval},
		{name: "negative", seconds: -1, expected: minAnnounceInterval},
		{name: "hugely negative", seconds: math.MinInt, expected: minAnnounceInterval},
		{name: "negative enough to wrap positive", seconds: wrapSeconds, expected: minAnnounceInterval},
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
		{name: "string length with no data behind it", data: unbackedStringResponse},
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

// allocationRuns is how many times allocatedBy runs the function it is measuring, taking the smallest measurement of
// the set as the one least polluted by allocations made elsewhere in the process while it ran.
const allocationRuns = 3

// allocatedBy returns an upper bound on the number of bytes f allocates. TotalAlloc counts the whole process, so a
// single difference across a call also carries whatever else was allocating while it ran — here that includes the
// httptest handler goroutine answering the very request being measured, which is charged to the call under test. That
// over-count can only make a bound on f's allocations fail, never pass wrongly, but it is still noise standing in for
// the thing being measured, so the smallest of several runs is taken: f allocates the same amount every time, while
// the noise alongside it does not.
func allocatedBy(f func()) uint64 {
	var smallest uint64
	for i := range allocationRuns {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		if allocated := after.TotalAlloc - before.TotalAlloc; i == 0 || allocated < smallest {
			smallest = allocated
		}
	}
	return smallest
}

// TestTrackerResponseIsBounded verifies that a tracker, which is an untrusted source, can neither hand us an
// unbounded response nor talk us into a huge allocation with a few bytes.
func TestTrackerResponseIsBounded(t *testing.T) {
	c := check.New(t)
	// Held behind a lock of its own, since it is written here between announces and read by the handler's goroutine,
	// which the shared httpClient's keep-alive connections have serving more than one of them
	response := &testTrackerResponse{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := response.get()
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	var tr tracker

	// A response declaring a 2GB string, with nothing behind it, must not be allocated for
	response.setBody(unbackedStringResponse)
	var err error
	allocated := allocatedBy(func() { _, err = fetchAnnounceResponse(&tr, srv.URL) })
	c.HasError(err)
	const allowed = 64 * 1024 * 1024
	c.True(allocated < allowed, "allocated %d bytes for a %d byte response", allocated,
		len(unbackedStringResponse))

	// A response larger than the cap is refused rather than read into memory
	response.setBody("d5:peers" + strconv.Itoa(2*maxTrackerResponseSize) + ":" +
		strings.Repeat("x", 2*maxTrackerResponseSize) + "e")
	_, err = fetchAnnounceResponse(&tr, srv.URL)
	c.HasError(err)

	// A normal response still decodes
	response.setBody("d8:intervali1800e8:completei2e10:incompletei1e5:peers6:" +
		string([]byte{10, 0, 0, 1, 0x1A, 0xE1}) + "e")
	in, err := fetchAnnounceResponse(&tr, srv.URL)
	c.NoError(err)
	c.Equal(1800, in.Interval)
	seeders, ok := swarmCount(in.Seeders)
	c.True(ok)
	c.Equal(2, seeders)
	leechers, ok := swarmCount(in.Leechers)
	c.True(ok)
	c.Equal(1, leechers)
	peers, err := parsePeers(discardLogger(), in.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}}, peers)
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

// TestAnnounceToleratesTheShutdownResponse verifies that whatever a tracker answers the stopped announce with is
// accepted without complaint, and that we stop considering ourselves started either way. Nothing in that response is
// ever read — the stop was delivered by the request itself — and trackers commonly reply to it with an empty body,
// plain text or a status other than 200 for exactly that reason, so anything less than a fully formed announce
// response must not turn the shutdown into a reported failure.
func TestAnnounceToleratesTheShutdownResponse(t *testing.T) {
	for _, one := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "empty dict", body: "de"},
		{name: "no interval", body: "d8:completei2ee"},
		{name: "zero interval", body: "d8:intervali0ee"},
		{name: "failure reason", body: failureResponse},
		{name: "nothing at all", body: ""},
		{name: "plain text", body: "ok"},
		{name: "truncated bencode", body: "d8:intervali1800"},
		{name: "a string with nothing behind it", body: unbackedStringResponse},
		{name: "not found", body: "404 page not found", status: http.StatusNotFound},
		{name: "server error", body: "", status: http.StatusInternalServerError},
		{name: "no content", body: "", status: http.StatusNoContent},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			d := newTestDispatcher(t)
			client, response := newTestTrackerClient(t, d)
			response.set(one.body, one.status)
			client.tracker.lock.Lock()
			client.tracker.started = true
			client.tracker.lock.Unlock()

			c.NoError(client.tracker.announce(context.Background(), stoppedMsg))
			c.False(client.tracker.hasStarted(), "the stop was delivered, so we are no longer started")
		})
	}

	// A stop that never reached the tracker is a different matter: the request is what carries the event, so a failure
	// to make it at all is still reported.
	c := check.New(t)
	d := newTestDispatcher(t)
	client, _ := newTestTrackerClient(t, d)
	client.torrentFile.Announce = unreachableTrackerURL
	client.tracker.lock.Lock()
	client.tracker.started = true
	client.tracker.lock.Unlock()
	c.HasError(client.tracker.announce(context.Background(), stoppedMsg))
}

// TestAFailedStartStillOwesAStoppedEvent verifies that a start announce the tracker answered counts as started even
// when the response turns out to be unusable. The tracker has us in its swarm from the moment it answers, so recording
// that only once the response has been made sense of leaves us registered with a tracker that is never told we left,
// handing our dead address to peers until its own timeout runs out.
func TestAFailedStartStillOwesAStoppedEvent(t *testing.T) {
	for _, one := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "no interval", body: "d5:peers0:e"},
		{name: "zero interval", body: "d8:intervali0e5:peers0:e"},
		{name: "an unreadable peer list", body: "d8:intervali1800e5:peersi5ee"},
		{name: "a failure reason", body: failureResponse},
		{name: "not bencode at all", body: "nope"},
		{name: "a status other than 200", body: noPeersResponse, status: http.StatusInternalServerError},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			d := newTestDispatcher(t)
			client, response := newTestTrackerClient(t, d)
			response.set(one.body, one.status)

			c.HasError(client.tracker.announce(context.Background(), startedMsg))
			c.True(client.tracker.hasStarted(), "the tracker answered, so it has us in its swarm")

			// Which means the stopped event is actually sent rather than quietly skipped
			response.set(noPeersResponse, 0)
			c.NoError(client.tracker.announceStopped())
			c.False(client.tracker.hasStarted())
		})
	}

	// A start that never reached the tracker leaves nothing to take back
	c := check.New(t)
	d := newTestDispatcher(t)
	client, _ := newTestTrackerClient(t, d)
	client.torrentFile.Announce = unreachableTrackerURL
	c.HasError(client.tracker.announce(context.Background(), startedMsg))
	c.False(client.tracker.hasStarted(), "a tracker that was never reached has no record of us")
	c.NoError(client.tracker.announceStopped())
}

// TestAnnounceKeepsTheIntervalWhenOneIsNotReturned verifies that an update or completion announce answered without an
// interval isn't treated as a failure and doesn't disturb the interval already in hand.
func TestAnnounceKeepsTheIntervalWhenOneIsNotReturned(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.setBody(noPeersResponse)

	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.True(client.tracker.hasStarted())
	c.Equal(30*time.Minute, client.tracker.announceInterval())

	response.setBody("d5:peers0:e")
	c.NoError(client.tracker.announce(context.Background(), "completed"))
	c.Equal(30*time.Minute, client.tracker.announceInterval())

	// A failure reason is still an error for anything but the shutdown announce
	response.setBody(failureResponse)
	c.HasError(client.tracker.announce(context.Background(), ""))
}

// TestAnnounceKeepsTheTrackerID verifies that a tracker id we were issued is sent back on every announce that
// follows, including the ones answered without one. A tracker that issues an id with the start response and leaves it
// out of the rest — which BEP 3 allows, and says we must go on returning the id we were given — would otherwise stop
// being told which session we are as soon as the next announce went out.
func TestAnnounceKeepsTheTrackerID(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)

	response.setBody("d8:intervali1800e5:peers0:10:tracker id5:abcdee")
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Contains(client.tracker.announceURL(""), "&trackerid=abcde")

	// A response that simply leaves the key out isn't taking the id away
	response.setBody(noPeersResponse)
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Contains(client.tracker.announceURL(""), "&trackerid=abcde")

	// A new id does replace it, and is escaped for the query it goes into
	response.setBody("d8:intervali1800e5:peers0:10:tracker id5:a b&ce")
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Contains(client.tracker.announceURL(""), "&trackerid=a%20b%26c")

	// A tracker that never issues one leaves the parameter out entirely
	client, response = newTestTrackerClient(t, d)
	response.setBody(noPeersResponse)
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.NotContains(client.tracker.announceURL(""), "trackerid")
}

// TestPercentEncodeEscapesEveryNonUnreservedByte verifies that an announce parameter carrying arbitrary bytes is
// percent-encoded rather than form-encoded. Form encoding, which is what url.QueryEscape does, writes the byte 0x20 as
// "+" and leaves a real "+" as %2B, so a tracker that percent-decodes the query string rather than form-decoding it
// cannot tell the two apart and reads a 0x2B where the 0x20 belongs.
func TestPercentEncodeEscapesEveryNonUnreservedByte(t *testing.T) {
	c := check.New(t)

	// Everything in RFC 3986's unreserved set, which is what the peer ID is already restricted to, passes through
	c.Equal(urlQuerySafeBytes, percentEncode(urlQuerySafeBytes))
	c.Equal("", percentEncode(""))

	// A space is %20 and never "+", and a literal "+" stays distinct from it
	c.Equal("a%20b", percentEncode("a b"))
	c.Equal("a%2Bb", percentEncode("a+b"))

	// Every other byte, the high ones a binary hash is full of included, becomes an uppercase %XX pair
	for i := range 256 {
		one := string([]byte{byte(i)})
		if strings.IndexByte(urlQuerySafeBytes, byte(i)) >= 0 {
			c.Equal(one, percentEncode(one), "byte 0x%02X is unreserved", i)
			continue
		}
		c.Equal(fmt.Sprintf("%%%02X", i), percentEncode(one), "byte 0x%02X must be percent-encoded", i)
	}

	// A plain percent-decode, which is all a tracker may do, hands every byte back unchanged
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	decoded, err := url.PathUnescape(percentEncode(string(all)))
	c.NoError(err)
	c.Equal(string(all), decoded)
}

// TestAnnounceURLPercentEncodesTheInfoHash verifies that an info hash holding bytes a query cannot carry literally
// reaches the tracker as %XX pairs. Roughly one hash in thirteen contains a 0x20 byte, and form encoding sends that as
// "+": a tracker doing a plain percent-decode then reconstructs a hash with 0x2B in its place, finds no such torrent
// and refuses the announce, leaving those torrents unable to reach it at all.
func TestAnnounceURLPercentEncodesTheInfoHash(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, _ := newTestTrackerClient(t, d)
	for i := range client.torrentFile.InfoHash {
		client.torrentFile.InfoHash[i] = byte(i + 100)
	}
	client.torrentFile.InfoHash[0] = ' '
	client.torrentFile.InfoHash[1] = '+'
	client.torrentFile.InfoHash[2] = '&'
	client.torrentFile.InfoHash[3] = 'a'

	urlStr := client.tracker.announceURL(startedMsg)
	c.Contains(urlStr, "info_hash=%20%2B%26a")

	// A tracker that form-decodes the query and one that only percent-decodes it must both recover the same 20 bytes
	parsed, err := url.Parse(urlStr)
	c.NoError(err)
	c.Equal(string(client.torrentFile.InfoHash[:]), parsed.Query().Get("info_hash"))
	decoded, err := url.PathUnescape(rawQueryValue(urlStr, "info_hash"))
	c.NoError(err)
	c.Equal(string(client.torrentFile.InfoHash[:]), decoded)

	// The same holds for a tracker id, which is likewise the tracker's own bytes handed back to it
	client.tracker.trackerID = "a b+c&d"
	urlStr = client.tracker.announceURL("")
	c.Contains(urlStr, "&trackerid=a%20b%2Bc%26d")
	decoded, err = url.PathUnescape(rawQueryValue(urlStr, "trackerid"))
	c.NoError(err)
	c.Equal(client.tracker.trackerID, decoded)
}

// TestAnnounceLogsThroughTheClientLogger verifies that the debug output an announce produces goes to the client's
// logger rather than the process default one. An application that pointed the client's logging somewhere of its own
// would otherwise never see these lines, and the announce URL — the tracker id it carries included — and the peer
// lists would land in the process-default sink at whatever level and format that happens to have.
func TestAnnounceLogsThroughTheClientLogger(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	sink := &logSink{}
	client.logger = slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defaultSink := captureDefaultLogger(t)

	// The compact peer list, which is what we ask for
	response.setBody("d8:intervali1800e5:peers6:" + string([]byte{10, 0, 0, 1, 0x1A, 0xE1}) + "e")
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Contains(sink.contents(), "url=")
	c.Contains(sink.contents(), "peers_list=")

	// The dict model, which a tracker is free to answer with instead
	raw, err := bencode.EncodeBytes(map[string]any{
		"interval": 1800,
		"peers":    []any{testPeerDict("", testPeerIP2, 6882)},
	})
	c.NoError(err)
	response.setBody(string(raw))
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Contains(sink.contents(), `msg="announce map"`)

	for _, one := range []string{"url=", "peers_list=", "announce map"} {
		c.NotContains(defaultSink.contents(), one,
			"announce must log through the client's logger, not the process default one")
	}
}

// rawQueryValue pulls one parameter's value out of a URL's query string without decoding it, which is what a tracker
// doing its own percent-decoding is handed.
func rawQueryValue(urlStr, key string) string {
	_, query, _ := strings.Cut(urlStr, "?")
	for _, one := range strings.Split(query, "&") {
		if value, ok := strings.CutPrefix(one, key+"="); ok {
			return value
		}
	}
	return ""
}

// TestStopWaitsForThePeriodicAnnounce verifies that the stopped announce isn't made while a periodic one is still
// under way. The two go out on connections of their own, so a tracker that finished with the update after the stopped
// event would have us back in its swarm for a full announce interval after we shut down.
func TestStopWaitsForThePeriodicAnnounce(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.setBody(noPeersResponse)
	c.NoError(client.tracker.announce(context.Background(), startedMsg))

	// A stand-in for the periodic announce goroutine, which the stop waits on exactly as it would the real one
	done := make(chan struct{})
	client.tracker.lock.Lock()
	client.tracker.periodicAnnounceDone = done
	client.tracker.lock.Unlock()

	stopped := make(chan error, 1)
	go func() { stopped <- client.tracker.announceStopped() }()
	select {
	case <-stopped:
		t.Fatal("the stopped announce went out while a periodic announce was still under way")
	case <-time.After(100 * time.Millisecond):
	}
	c.True(client.tracker.hasStarted(), "the stopped announce must not have been made yet")

	// Once that announce is done with, so is the wait
	close(done)
	select {
	case err := <-stopped:
		c.NoError(err)
	case <-time.After(peerMgmtWait):
		t.Fatal("the stopped announce never went out")
	}
	c.False(client.tracker.hasStarted())
}

// TestStopAbortsTheAnnounceInFlight verifies that the announce the periodic goroutine is in the middle of is cut short
// rather than left to run out the HTTP timeout. The stopped announce, the close of the storage file and the stopped
// notification all queue behind the wait for that goroutine, so without the cancellation the shutdown would sit on a
// round trip with up to 30 seconds left in it — far beyond the timeout the caller gave Stop.
func TestStopAbortsTheAnnounceInFlight(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)

	// A tracker that never answers, leaving the announce parked on the response for as long as it is allowed
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-held:
		}
	}))
	defer srv.Close()
	defer close(held)
	client, _ := newTestTrackerClient(t, d)
	client.torrentFile.Announce = srv.URL

	// The periodic announce goroutine makes its announces with the context the stop cancels
	returned := make(chan error, 1)
	go func() { returned <- client.tracker.announce(client.tracker.announceCtx, "") }()
	select {
	case <-returned:
		t.Fatal("the announce came back before the tracker answered it")
	case <-time.After(100 * time.Millisecond):
	}

	client.tracker.stopPeriodicAnnounce()
	select {
	case err := <-returned:
		c.HasError(err, "the announce was cut short, which is not a success")
	case <-time.After(peerMgmtWait):
		t.Fatal("the announce in flight was left to run out the HTTP timeout")
	}
	c.True(client.tracker.announceStopping())
}

// TestStopAbortsTheStartAnnounce verifies that a Stop arriving while the start announce is parked on an unresponsive
// tracker cuts it short, the way it can every later announce. Left uncancelable, run() stays inside the start for up
// to the HTTP client's 30 second timeout, and the stopped announce, the close of the storage file and the stopped
// notification all land well past the bound Stop's caller asked for.
func TestStopAbortsTheStartAnnounce(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)

	// A tracker that never answers, leaving the announce parked on the response for as long as it is allowed
	held := make(chan struct{})
	reached := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-held:
		}
	}))
	defer srv.Close()
	defer close(held)
	client, _ := newTestTrackerClient(t, d)
	client.torrentFile.Announce = srv.URL

	returned := make(chan error, 1)
	go func() { returned <- client.tracker.announceStart() }()
	select {
	case <-reached:
	case <-time.After(peerMgmtWait):
		t.Fatal("the start announce never reached the tracker")
	}
	select {
	case <-returned:
		t.Fatal("the start announce came back before the tracker answered it")
	case <-time.After(100 * time.Millisecond):
	}

	// Which is what a Stop has to be able to cut short. No wait is asked of it, since this client's run goroutine was
	// never started and so has nothing to report finishing; signaling the stop is the whole of what is being tested.
	client.Stop(0)
	select {
	case err := <-returned:
		c.True(errors.Is(err, errStopRequested),
			"a start announce cut short by the stop is the requested stop, not a torrent that errored: %v", err)
	case <-time.After(peerMgmtWait):
		t.Fatal("the start announce was left to run out the HTTP timeout")
	}
	c.False(client.tracker.hasStarted(), "an announce that never reached the tracker leaves us out of the swarm")
}

// TestAStartAnnounceIsAppliedWithTheStopAlreadyUnderWay verifies the exemption the shutdown check makes for the start
// announce. Every other announce whose response lands after the stop was signaled is dropped, since what it describes
// belongs to a client that no longer exists, but the two genuinely overlap for the start: Client.Stop cancels the
// announce context from another goroutine for the express purpose of cutting a start announce short, so a response that
// wins the race against the cancellation is made sense of with the stop already under way. Dropping it there would
// leave announceStart going on to start the periodic announce with no interval to run at.
func TestAStartAnnounceIsAppliedWithTheStopAlreadyUnderWay(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.setBody(oneCompactPeerResponse)

	// The stop has been signaled, which is all cancelAnnounce amounts to, while the announce is still in flight. Its
	// request is made with a context of its own, so that it is the answer that wins the race rather than the
	// cancellation, which is exactly the case the exemption is about.
	client.tracker.cancelAnnounce()
	c.True(client.tracker.announceStopping())
	c.NoError(client.tracker.announce(context.Background(), startedMsg))

	client.tracker.lock.RLock()
	interval := client.tracker.interval
	peers := client.tracker.peerAddresses
	client.tracker.lock.RUnlock()
	c.Equal(1800, interval, "the periodic announce has nothing to run at without the interval the start came back with")
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}}, peers, "the swarm the start announce found must not be discarded")
	c.True(client.tracker.hasStarted(),
		"the started flag recorded when the tracker answered is what says a stopped event is owed")

	// Everything that isn't the start announce is dropped instead, since by the time one of those comes back what it
	// describes belongs to a client that is already gone
	client.tracker.lock.Lock()
	client.tracker.interval = 0
	client.tracker.peerAddresses = nil
	client.tracker.lock.Unlock()
	c.NoError(client.tracker.announce(context.Background(), ""))
	client.tracker.lock.RLock()
	interval = client.tracker.interval
	peers = client.tracker.peerAddresses
	client.tracker.lock.RUnlock()
	c.Equal(0, interval, "an update the stop overtook must not be applied")
	c.Equal(0, len(peers))
}

// TestAnnounceBodyDiscardIsBounded verifies that draining what is left of a tracker's answer takes no more than the cap
// every response is held to. The drain runs on the paths that never read the body at all — the stopped announce, an
// answer carrying a failure reason, one already refused for being too large — so an uncapped one hands a hostile
// tracker a way to keep the announce goroutine streaming discarded bytes until the HTTP client's timeout runs out,
// which is the whole of what the cap exists to prevent.
func TestAnnounceBodyDiscardIsBounded(t *testing.T) {
	c := check.New(t)
	body := &endlessBody{}
	discardAndCloseAnnounceBody(body)
	c.True(body.read <= maxTrackerResponseSize+1, "%d bytes were read of an endless response", body.read)
	c.True(body.closed, "the body must be closed whatever it held")
}

// endlessBody stands in for the response of a tracker that never stops sending, counting what was taken from it.
type endlessBody struct {
	read   int
	closed bool
}

func (b *endlessBody) Read(p []byte) (int, error) {
	b.read += len(p)
	return len(p), nil
}

func (b *endlessBody) Close() error {
	b.closed = true
	return nil
}

// TestPeriodicAnnounceStopsWhenTold verifies that the goroutine returns on the stop and closes the channel the stop
// waits on, rather than the two waiting on each other.
func TestPeriodicAnnounceStopsWhenTold(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.setBody(noPeersResponse)

	client.tracker.startPeriodicAnnounce()
	client.tracker.lock.RLock()
	done := client.tracker.periodicAnnounceDone
	client.tracker.lock.RUnlock()
	c.NotNil(done)

	waitFor(t, "stopPeriodicAnnounce", client.tracker.stopPeriodicAnnounce)
	select {
	case <-done:
	default:
		t.Fatal("the stop returned before the periodic announce had finished")
	}

	// A second stop is harmless, which matters because the client stops the announces on every path out
	waitFor(t, "stopPeriodicAnnounce", client.tracker.stopPeriodicAnnounce)
}

// TestAnnounceOvertakenByTheStopIsNotApplied verifies that what a periodic announce comes back with after the shutdown
// has begun is discarded. It describes a swarm we're on our way out of, so letting the peer list, the interval and the
// counts we report land after the stopped event leaves all of them belonging to a client that no longer exists.
func TestAnnounceOvertakenByTheStopIsNotApplied(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.setBody(noPeersResponse)
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Equal(0, len(client.tracker.knownPeers()))

	// The announce is made with a context of its own, standing in for a request that was already on the wire when the
	// stop was signaled and only came back afterwards
	client.tracker.stopPeriodicAnnounce()
	response.setBody("d8:intervali3600e8:completei9e5:peers6:" + string([]byte{10, 0, 0, 1, 0x1A, 0xE1}) + "e")
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Equal(0, len(client.tracker.knownPeers()), "the peer list of a swarm we have left must not be taken up")
	c.Equal(30*time.Minute, client.tracker.announceInterval())
	c.Equal(0, client.Status().Seeders)
}

// TestParsePeersRejectsUnusablePorts verifies that the dict model's port is checked against the port range before it
// becomes something we dial. It arrives as a bencoded integer, unlike the compact form's inherently 16-bit value, so a
// hostile or buggy tracker can fill the peer list with ports no dial can ever reach, each of them attempted again on
// every peer management pass until the next announce replaces the list.
func TestParsePeersRejectsUnusablePorts(t *testing.T) {
	c := check.New(t)
	in := decodeTestTrackerResponse(t, []any{
		testPeerDict("", testPeerIP1, 6881),
		testPeerDict("", testPeerIP2, 1),
		testPeerDict("", testPeerIP3, math.MaxUint16),
		testPeerDict("", "10.0.0.4", 0),
		testPeerDict("", "10.0.0.5", -1),
		testPeerDict("", "10.0.0.6", -6881),
		testPeerDict("", "10.0.0.7", math.MaxUint16+1),
		testPeerDict("", "10.0.0.8", math.MaxInt32),
	})
	peers, err := parsePeers(discardLogger(), in.PeerAddresses, "<unknown>", 0, testPeerLimit)
	c.NoError(err)
	c.Equal([]peerAddr{
		{ip: testPeerIP1, port: 6881},
		{ip: testPeerIP2, port: 1},
		{ip: testPeerIP3, port: math.MaxUint16},
	}, peers)
	for _, addr := range []string{"10.0.0.4", "10.0.0.5", "10.0.0.6", "10.0.0.7", "10.0.0.8"} {
		c.False(slices.ContainsFunc(peers, func(one peerAddr) bool { return one.ip == addr }),
			"%s has a port outside the range a dial can use", addr)
	}
}

// TestCompletedAnnounceIsNotMadeOnThePeerGoroutine verifies that finishing the download hands the "completed" announce
// to the announce goroutine rather than making it where the last piece landed. That is a peer's message loop, which
// the client's peer wait group tracks, so an announce made there stalls the peer for a full tracker round trip and, if
// a stop arrives while it is in flight, holds the shutdown — the stopped announce, the close of the storage file and
// the stopped notification all queue behind the wait for that peer — for as long as the HTTP client's timeout allows.
func TestCompletedAnnounceIsNotMadeOnThePeerGoroutine(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	events := make(chan string, 4)
	held := make(chan struct{})
	defer close(held)
	client := newTestTrackerClientWithHandler(t, d, func(w http.ResponseWriter, r *http.Request) {
		event := r.URL.Query().Get("event")
		events <- event
		if event == completedMsg {
			// The completion is left hanging, standing in for a tracker that is slow to answer
			select {
			case <-held:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, noPeersResponse)
	})
	c.NoError(client.tracker.announceStart())
	c.Equal(startedMsg, nextAnnounceValue(t, events))

	// Completing the download has to come back at once rather than sit on the tracker's answer
	waitFor(t, "markBlockValid", func() {
		for i := range client.torrentFile.PieceCount() {
			client.tracker.markBlockValid(i)
		}
	})
	c.True(client.tracker.isDownloadComplete())

	// The announce is still made, just not from there
	select {
	case event := <-events:
		c.Equal(completedMsg, event)
	case <-time.After(peerMgmtWait):
		t.Fatal("the completed announce was never made")
	}

	// And being on that goroutine puts it under the same cancellation as every other announce, so the stop cuts the
	// round trip it is parked on short instead of leaving the shutdown to wait out the HTTP timeout
	client.tracker.stopPeriodicAnnounce()
	client.tracker.lock.RLock()
	done := client.tracker.periodicAnnounceDone
	client.tracker.lock.RUnlock()
	select {
	case <-done:
	default:
		t.Fatal("the completed announce in flight was not cut short by the stop")
	}
}

// TestCompletedIsNotAnnouncedForADownloadThatNeverHappened verifies that a client coming up with a complete file on
// disk doesn't report a completion. Verifying that file marks every piece valid, which is what would ask for the
// announce, but it happens before the start announce and the event describes a torrent downloaded during this session.
func TestCompletedIsNotAnnouncedForADownloadThatNeverHappened(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	events := make(chan string, 4)
	client := newTestTrackerClientWithHandler(t, d, func(w http.ResponseWriter, r *http.Request) {
		events <- r.URL.Query().Get("event")
		fmt.Fprint(w, noPeersResponse)
	})

	// Which is what verifying an already complete file on disk does, before anything has been announced at all
	for i := range client.torrentFile.PieceCount() {
		client.tracker.markBlockValid(i)
	}
	c.True(client.tracker.isDownloadComplete())

	c.NoError(client.tracker.announceStart())
	c.Equal(startedMsg, nextAnnounceValue(t, events))
	defer client.tracker.stopPeriodicAnnounce()
	select {
	case event := <-events:
		t.Fatalf("a %q event was announced for a download that never happened", event)
	case <-time.After(100 * time.Millisecond):
	}
}

// nextAnnounceValue returns what the stub tracker recorded for the next announce it is asked to make. The wait is
// bounded because the announce that would record one may fail before it ever reaches the stub: a bare receive would
// then block for as long as the test binary is allowed to run, turning what should be one failed assertion into a
// timeout panic that takes the whole package's test run down with it.
func nextAnnounceValue(t *testing.T, recorded <-chan string) string {
	t.Helper()
	select {
	case value := <-recorded:
		return value
	case <-time.After(peerMgmtWait):
		t.Fatal("no announce reached the tracker")
		return ""
	}
}

// TestTrackerUserAgentIsSafeToSetWhileAnnouncing verifies that the user agent reaches the tracker and that setting it
// while announces are being made is not a data race. Announces are made from the client's run goroutine and from each
// tracker's periodic announce goroutine, neither of which an application setting a package-level knob knows anything
// about, so nothing but the value's own synchronization stands between the two.
func TestTrackerUserAgentIsSafeToSetWhileAnnouncing(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	agents := make(chan string, 2*concurrentAgentSetters+2)
	client := newTestTrackerClientWithHandler(t, d, func(w http.ResponseWriter, r *http.Request) {
		agents <- r.UserAgent()
		fmt.Fprint(w, noPeersResponse)
	})
	original := TrackerUserAgent()
	defer SetTrackerUserAgent(original)

	// An empty agent, which is the default, sets no header of our own, leaving the HTTP client's to stand
	SetTrackerUserAgent("")
	c.Equal("", TrackerUserAgent())
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.NotContains(nextAnnounceValue(t, agents), testUserAgent)

	// What was set is what the tracker sees
	SetTrackerUserAgent(testUserAgent)
	c.Equal(testUserAgent, TrackerUserAgent())
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Equal(testUserAgent, nextAnnounceValue(t, agents))

	// And setting it alongside announces already in flight is something an application is free to do, which is what
	// the race detector is here to judge
	var wg sync.WaitGroup
	for i := range concurrentAgentSetters {
		wg.Add(2)
		go func() {
			defer wg.Done()
			SetTrackerUserAgent(fmt.Sprintf("%s/%d", testUserAgent, i))
		}()
		go func() {
			defer wg.Done()
			// Whether the announce itself succeeds isn't what is being judged here, only that reading the agent
			// alongside a set of it is safe.
			_ = client.tracker.announce(context.Background(), "") //nolint:errcheck // See above
		}()
	}
	wg.Wait()
	c.HasPrefix(TrackerUserAgent(), testUserAgent)
}

// testTrackerResponse is what the stub tracker answers with. It may be changed between announces; a status of zero
// means the ordinary 200.
//
// The fields are written by the test goroutine and read by the httptest handler's, and the shared httpClient keeps its
// connections alive, so the same server goroutine reads them across requests: without synchronization of their own
// that is a data race under the Go memory model, which the race detector flags whenever the two happen to overlap.
type testTrackerResponse struct {
	body   string
	status int
	lock   sync.RWMutex
}

// set records what the stub tracker will answer the announces that follow with.
func (r *testTrackerResponse) set(body string, status int) {
	r.lock.Lock()
	r.body = body
	r.status = status
	r.lock.Unlock()
}

// setBody records the body the stub tracker will answer the announces that follow with, leaving the status as the
// ordinary 200.
func (r *testTrackerResponse) setBody(body string) {
	r.set(body, 0)
}

// get returns what the stub tracker should answer the announce it is handling with.
func (r *testTrackerResponse) get() (body string, status int) {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.body, r.status
}

// newTestTrackerClient returns a client whose announces go to a stub tracker, along with the response that stub will
// return.
func newTestTrackerClient(t *testing.T, d *dispatcher.Dispatcher) (client *Client, response *testTrackerResponse) {
	t.Helper()
	response = &testTrackerResponse{}
	return newTestTrackerClientWithHandler(t, d, func(w http.ResponseWriter, _ *http.Request) {
		body, status := response.get()
		if status != 0 {
			w.WriteHeader(status)
		}
		fmt.Fprint(w, body)
	}), response
}

// newTestTrackerClientWithHandler returns a client whose announces go to a stub tracker answering them with the given
// handler, for the tests that need to look at the request or hold the answer back. The peer ID is filled in with
// query-safe bytes, which is what NewClient does and what the announce URL relies on.
func newTestTrackerClientWithHandler(t *testing.T, d *dispatcher.Dispatcher, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := newTestClient(d)
	client.torrentFile.Announce = srv.URL
	for i := range client.id {
		client.id[i] = urlQuerySafeBytes[i%len(urlQuerySafeBytes)]
	}
	return client
}

// discardLogger is the logger handed to parsePeers by the tests that care about the peer list it returns rather than
// what it logged along the way.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// unreachableTrackerURL is an announce URL that no request can ever be made to, standing in for a tracker that never
// hears from us at all. It is malformed rather than merely unroutable, so that it fails at once instead of waiting out
// a connection attempt.
const unreachableTrackerURL = "http://%zz/announce"

// fetchAnnounceResponse makes an announce request and reads what comes back the way an announce does, so that the two
// halves the announce keeps apart can be exercised together.
func fetchAnnounceResponse(tr *tracker, urlStr string) (*trackerWire, error) {
	resp, err := tr.request(context.Background(), urlStr)
	if err != nil {
		return nil, err
	}
	defer discardAndCloseAnnounceBody(resp.Body)
	return readAnnounceResponse(resp)
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

// TestMarkBlockValidAnnouncesAPieceOnce verifies that a piece we already hold isn't announced to our peers a second
// time. Everything else markBlockValid does sits behind the guard that asks whether the piece is new; the "have"
// message used to sit outside it, so a repeat call — which the claim bookkeeping makes unlikely rather than
// impossible, a piece being claimable twice — queued a duplicate message to every peer we have.
func TestMarkBlockValidAnnouncesAPieceOnce(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client := newTestClient(d)
	conn, p := newTestPeerFromHost(t, client, testPeerHost1, testPeerPort)
	defer xio.CloseIgnoringErrors(conn)

	client.tracker.markBlockValid(0)
	client.tracker.markBlockValid(0)

	p.lock.RLock()
	queued := slices.Clone(p.haveQueue)
	p.lock.RUnlock()
	c.Equal([]int{0}, queued, "a piece we already hold must not be announced again")

	// The rest of the state change is unaffected by the repeat as well, so the remaining count is decremented once.
	c.True(client.tracker.hasPiece(0))
	client.tracker.lock.RLock()
	remaining := client.tracker.remainingBytes
	client.tracker.lock.RUnlock()
	c.Equal(client.torrentFile.Size()-client.torrentFile.LengthOf(0), remaining)
}

// TestCompletionDuringTheStartAnnounceIsStillReported verifies that a download which finishes while the start announce
// is in flight still reports its completion. The client registers with the dispatcher before announcing, so an inbound
// peer can deliver the last pieces during that round trip — up to the HTTP client's 30 second timeout, and a nearly
// complete torrent resumed from disk needs very little — at which point the gate that keeps a completion from being
// reported before we ever started used to drop it, with nothing to re-check afterwards.
func TestCompletionDuringTheStartAnnounceIsStillReported(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	events := make(chan string, 4)
	announcing := make(chan struct{})
	release := make(chan struct{})
	client := newTestTrackerClientWithHandler(t, d, func(w http.ResponseWriter, r *http.Request) {
		event := r.URL.Query().Get("event")
		if event == startedMsg {
			// The tracker is slow to answer, which is the window an inbound peer delivers pieces in
			close(announcing)
			<-release
		}
		events <- event
		fmt.Fprint(w, noPeersResponse)
	})

	// Which is the state the client is in from the moment it registers with the dispatcher, before it announces
	client.tracker.enterSwarm()
	go func() {
		<-announcing
		for i := range client.torrentFile.PieceCount() {
			client.tracker.markBlockValid(i)
		}
		close(release)
	}()

	c.NoError(client.tracker.announceStart())
	defer client.tracker.stopPeriodicAnnounce()
	c.True(client.tracker.isDownloadComplete())
	c.Equal(startedMsg, nextAnnounceValue(t, events))
	c.Equal(completedMsg, nextAnnounceValue(t, events),
		"a download finished while the start announce was in flight still owes the tracker a completed event")
}

// TestIsDialableRefusesAddressesThatArentPeers verifies which hosts a tracker's peer list can steer a dial at. Whoever
// supplies the .torrent chooses the tracker and the tracker chooses the peers, so an entry is an address of their
// choosing that we open a TCP connection to and write a handshake carrying 20 bytes they also chose; the addresses
// that name something other than a peer out on the network are refused here rather than dialed.
func TestIsDialableRefusesAddressesThatArentPeers(t *testing.T) {
	for _, one := range []struct {
		name     string
		ip       string
		dialable bool
	}{
		{name: "no host at all", ip: ""},
		{name: "the unspecified IPv4 address", ip: "0.0.0.0"},
		{name: "the unspecified IPv6 address", ip: "::"},
		{name: "IPv4 loopback", ip: "127.0.0.1"},
		{name: "elsewhere in the IPv4 loopback range", ip: "127.1.2.3"},
		{name: "IPv6 loopback", ip: "::1"},
		{name: "IPv4 link-local", ip: "169.254.1.1"},
		{name: "IPv6 link-local", ip: "fe80::1"},
		{name: "IPv4 multicast", ip: "224.0.0.1"},
		{name: "IPv6 multicast", ip: "ff02::1"},
		{name: "the IPv4 broadcast address", ip: "255.255.255.255"},
		{name: "this network", ip: "0.1.2.3"},
		{name: "a public address", ip: "203.0.113.7", dialable: true},
		{name: "a private address on a LAN swarm", ip: testPeerIP1, dialable: true},
		{name: "another private range", ip: "192.168.1.10", dialable: true},
		{name: "an IPv6 address of its own", ip: "2001:db8::1", dialable: true},
		{name: "a name rather than an address", ip: "peer.example.com", dialable: true},
	} {
		t.Run(one.name, func(t *testing.T) {
			check.New(t).Equal(one.dialable, peerAddr{ip: one.ip, port: 6881}.isDialable())
		})
	}
}

// compactPeerList builds a compact peer list holding the given number of distinct, dialable entries.
func compactPeerList(count int) string {
	var buffer strings.Builder
	for i := range count {
		buffer.Write([]byte{10, 1, byte(i / 256), byte(i % 256), 0x1A, 0xE1})
	}
	return buffer.String()
}

// TestParsePeersIsBounded verifies that the number of peers a tracker can put on us is limited. Only the size of the
// response was bounded before, and a megabyte of compact entries — which that bound allows — is about 174,000 peers,
// every one of them retained for the life of the torrent and walked, with a gatekeeper lookup and a log line apiece,
// on each fifteen-second peer adjustment. The announce says how many we want; nothing makes a tracker honor it.
func TestParsePeersIsBounded(t *testing.T) {
	c := check.New(t)
	const limit = 8

	// The compact form, which is what we ask for
	peers, err := parsePeers(discardLogger(), decodeTestTrackerResponse(t, compactPeerList(limit+5)).PeerAddresses,
		"<unknown>", 0, limit)
	c.NoError(err)
	c.Equal(limit, len(peers))

	// And the dict model, which a tracker may answer with regardless
	list := make([]any, 0, limit+5)
	for i := range limit + 5 {
		list = append(list, testPeerDict("", fmt.Sprintf("10.1.0.%d", i+1), 6881))
	}
	peers, err = parsePeers(discardLogger(), decodeTestTrackerResponse(t, list).PeerAddresses, "<unknown>", 0, limit)
	c.NoError(err)
	c.Equal(limit, len(peers))

	// A list within the limit is untouched
	peers, err = parsePeers(discardLogger(), decodeTestTrackerResponse(t, compactPeerList(limit)).PeerAddresses,
		"<unknown>", 0, limit)
	c.NoError(err)
	c.Equal(limit, len(peers))
}

// TestAnnounceBoundsThePeerList verifies the same limit where it actually applies: what an announce retains for the
// peer adjustments to walk, which is a multiple of the number of peers we asked the tracker for.
func TestAnnounceBoundsThePeerList(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	client.peersWanted = 2
	limit := client.tracker.peerListLimit()
	c.Equal(2*maxPeerListOverage, limit)

	response.setBody(fmt.Sprintf("d8:intervali1800e5:peers%d:%se", 6*(limit+10), compactPeerList(limit+10)))
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Equal(limit, len(client.tracker.knownPeers()))
}

// TestParsePeersRejectsACompactListOfTheWrongLength verifies that a compact list which isn't whole 6-byte entries is
// refused rather than half-read. What it cannot catch is BEP-7's 18-byte IPv6 entries put under the IPv4 key: 18
// divides by 6, so such a list looks exactly like three times as many IPv4 entries. Nothing those entries misparse
// into survives isDialable, which is what keeps them from becoming dials.
func TestParsePeersRejectsACompactListOfTheWrongLength(t *testing.T) {
	c := check.New(t)

	for _, one := range []struct {
		name  string
		value string
	}{
		{name: "a trailing partial entry", value: compactPeerList(2) + "\x0a\x00\x00"},
		{name: "a single byte", value: "\x0a"},
		{name: "one byte short of an entry", value: compactPeerList(1)[:5]},
	} {
		t.Run(one.name, func(t *testing.T) {
			_, err := parsePeers(discardLogger(), decodeTestTrackerResponse(t, one.value).PeerAddresses, "<unknown>",
				0, testPeerLimit)
			check.New(t).HasError(err)
		})
	}

	// A compact IPv6 entry — 2001:db8::1 on port 6881 — under the IPv4 key: the length says nothing, and every IPv4
	// address it is read as is one nothing can be reached at
	ipv6 := string([]byte{
		0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01, 0x1A, 0xE1,
	})
	peers, err := parsePeers(discardLogger(), decodeTestTrackerResponse(t, ipv6).PeerAddresses, "<unknown>", 0,
		testPeerLimit)
	c.NoError(err)
	c.Equal(0, len(peers), "an IPv6 entry misread as IPv4 must not yield anything dialable")
}

// TestAnnounceKeepsThePeerListWhenNoneIsReturned verifies that a response which simply leaves the peers key out isn't
// read as an empty swarm. Cleared there, the client has nothing left to dial and no way to replace a lost connection
// until the next announce, which the interval allows to be as far off as a day.
func TestAnnounceKeepsThePeerListWhenNoneIsReturned(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)

	response.setBody(oneCompactPeerResponse)
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}}, client.tracker.knownPeers())

	// No peers key at all, so the swarm we know about is still the one we knew about
	response.setBody("d8:intervali1800ee")
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Equal([]peerAddr{{ip: testPeerIP1, port: 6881}}, client.tracker.knownPeers())

	// An explicitly empty list is a different statement, and does clear it
	response.setBody(noPeersResponse)
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Equal(0, len(client.tracker.knownPeers()))
}

// TestAnnounceKeepsTheSwarmCountsWhenNoneAreReturned verifies that the seeder and leecher counts survive a response
// that doesn't carry them, and that a nonsense count never reaches the Status a caller reads. The "complete" and
// "incomplete" keys are optional and commonly left out, so applying them unconditionally has every such response
// silently report an empty swarm, and a tracker that is buggy or hostile can put a negative number in front of whoever
// is watching the download.
func TestAnnounceKeepsTheSwarmCountsWhenNoneAreReturned(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)

	response.setBody("d8:intervali1800e8:completei7e10:incompletei3e5:peers0:e")
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	status := client.tracker.status(0, 0)
	c.Equal(7, status.Seeders)
	c.Equal(3, status.Leechers)

	// No counts at all, so the swarm we know about is still the one we knew about
	response.setBody(noPeersResponse)
	c.NoError(client.tracker.announce(context.Background(), ""))
	status = client.tracker.status(0, 0)
	c.Equal(7, status.Seeders)
	c.Equal(3, status.Leechers)

	// A count no swarm can have is refused rather than reported
	response.setBody("d8:intervali1800e8:completei-1e10:incompletei-2e5:peers0:e")
	c.NoError(client.tracker.announce(context.Background(), ""))
	status = client.tracker.status(0, 0)
	c.Equal(7, status.Seeders)
	c.Equal(3, status.Leechers)

	// An explicit zero is a statement about the swarm, and is recorded like any other count
	response.setBody("d8:intervali1800e8:completei0e10:incompletei0e5:peers0:e")
	c.NoError(client.tracker.announce(context.Background(), ""))
	status = client.tracker.status(0, 0)
	c.Equal(0, status.Seeders)
	c.Equal(0, status.Leechers)
}

// TestAnnounceReportsAnIPv6PeerListItCannotUse verifies that a tracker answering under BEP-7's peers6 key is heard
// rather than ignored. This client announces and dials IPv4, so those peers can't be used, but a response holding
// nothing else would otherwise leave it with no one to talk to and nothing said about why.
func TestAnnounceReportsAnIPv6PeerListItCannotUse(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	sink := &logSink{}
	client.logger = slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 2001:db8::1 and 2001:db8::2, both on port 6881, in the key BEP-7 gives them
	ipv6 := string([]byte{
		0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01, 0x1A, 0xE1,
		0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x02, 0x1A, 0xE1,
	})
	response.setBody(fmt.Sprintf("d8:intervali1800e5:peers0:6:peers6%d:%se", len(ipv6), ipv6))
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Equal(0, len(client.tracker.knownPeers()))
	c.Contains(sink.contents(), "BEP-7")

	// A response carrying only IPv4 peers says nothing about IPv6
	quiet := &logSink{}
	client.logger = slog.New(slog.NewTextHandler(quiet, &slog.HandlerOptions{Level: slog.LevelDebug}))
	response.setBody(oneCompactPeerResponse)
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.NotContains(quiet.contents(), "BEP-7")
}
