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
	"github.com/richardwilkes/toolbox/v2/xio"
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

const (
	// noPeersResponse is a tracker response carrying an interval and an empty peer list, which is everything an
	// announce needs and nothing more.
	noPeersResponse = "d8:intervali1800e5:peers0:e"

	// unbackedStringResponse declares a 2GB string with nothing at all behind it, which is what a tracker hands us when
	// it wants us to allocate 2GB.
	unbackedStringResponse = "d5:peers2147483646:"

	// failureResponse is a tracker refusing the announce, the one thing in a response that is an error in its own
	// right rather than something missing from it.
	failureResponse = "d14:failure reason9:not founde"
)

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
	body = unbackedStringResponse
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := fetchAnnounceResponse(&tr, srv.URL)
	runtime.ReadMemStats(&after)
	c.HasError(err)
	const allowed = 64 * 1024 * 1024
	c.True(after.TotalAlloc-before.TotalAlloc < allowed, "allocated %d bytes for a %d byte response",
		after.TotalAlloc-before.TotalAlloc, len(body))

	// A response larger than the cap is refused rather than read into memory
	body = "d5:peers" + strconv.Itoa(2*maxTrackerResponseSize) + ":" + strings.Repeat("x", 2*maxTrackerResponseSize) + "e"
	_, err = fetchAnnounceResponse(&tr, srv.URL)
	c.HasError(err)

	// A normal response still decodes
	body = "d8:intervali1800e8:completei2e10:incompletei1e5:peers6:" + string([]byte{10, 0, 0, 1, 0x1A, 0xE1}) + "e"
	in, err := fetchAnnounceResponse(&tr, srv.URL)
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
			response.body = one.body
			response.status = one.status
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
			response.body = one.body
			response.status = one.status

			c.HasError(client.tracker.announce(context.Background(), startedMsg))
			c.True(client.tracker.hasStarted(), "the tracker answered, so it has us in its swarm")

			// Which means the stopped event is actually sent rather than quietly skipped
			response.body = noPeersResponse
			response.status = 0
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
	response.body = noPeersResponse

	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.True(client.tracker.hasStarted())
	c.Equal(30*time.Minute, client.tracker.announceInterval())

	response.body = "d5:peers0:e"
	c.NoError(client.tracker.announce(context.Background(), "completed"))
	c.Equal(30*time.Minute, client.tracker.announceInterval())

	// A failure reason is still an error for anything but the shutdown announce
	response.body = failureResponse
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

	response.body = "d8:intervali1800e5:peers0:10:tracker id5:abcdee"
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Contains(client.tracker.announceURL(""), "&trackerid=abcde")

	// A response that simply leaves the key out isn't taking the id away
	response.body = noPeersResponse
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Contains(client.tracker.announceURL(""), "&trackerid=abcde")

	// A new id does replace it, and is escaped for the query it goes into
	response.body = "d8:intervali1800e5:peers0:10:tracker id5:a b&ce"
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Contains(client.tracker.announceURL(""), "&trackerid=a+b%26c")

	// A tracker that never issues one leaves the parameter out entirely
	client, response = newTestTrackerClient(t, d)
	response.body = noPeersResponse
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.NotContains(client.tracker.announceURL(""), "trackerid")
}

// TestStopWaitsForThePeriodicAnnounce verifies that the stopped announce isn't made while a periodic one is still
// under way. The two go out on connections of their own, so a tracker that finished with the update after the stopped
// event would have us back in its swarm for a full announce interval after we shut down.
func TestStopWaitsForThePeriodicAnnounce(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.body = noPeersResponse
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

// TestPeriodicAnnounceStopsWhenTold verifies that the goroutine returns on the stop and closes the channel the stop
// waits on, rather than the two waiting on each other.
func TestPeriodicAnnounceStopsWhenTold(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	client, response := newTestTrackerClient(t, d)
	response.body = noPeersResponse

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
	response.body = noPeersResponse
	c.NoError(client.tracker.announce(context.Background(), startedMsg))
	c.Equal(0, len(client.tracker.peerAddressesMap()))

	// The announce is made with a context of its own, standing in for a request that was already on the wire when the
	// stop was signaled and only came back afterwards
	client.tracker.stopPeriodicAnnounce()
	response.body = "d8:intervali3600e8:completei9e5:peers6:" + string([]byte{10, 0, 0, 1, 0x1A, 0xE1}) + "e"
	c.NoError(client.tracker.announce(context.Background(), ""))
	c.Equal(0, len(client.tracker.peerAddressesMap()), "the peer list of a swarm we have left must not be taken up")
	c.Equal(30*time.Minute, client.tracker.announceInterval())
	c.Equal(0, client.Status().Seeders)
}

// testTrackerResponse is what the stub tracker answers with. Either field may be changed between announces; a status
// of zero means the ordinary 200.
type testTrackerResponse struct {
	body   string
	status int
}

// newTestTrackerClient returns a client whose announces go to a stub tracker, along with the response that stub will
// return. The peer ID is filled in with query-safe bytes, which is what NewClient does and what the announce URL
// relies on.
func newTestTrackerClient(t *testing.T, d *dispatcher.Dispatcher) (client *Client, response *testTrackerResponse) {
	t.Helper()
	response = &testTrackerResponse{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if response.status != 0 {
			w.WriteHeader(response.status)
		}
		fmt.Fprint(w, response.body)
	}))
	t.Cleanup(srv.Close)
	client = newTestClient(d)
	client.torrentFile.Announce = srv.URL
	for i := range client.id {
		client.id[i] = urlQuerySafeBytes[i%len(urlQuerySafeBytes)]
	}
	return client, response
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
	defer xio.DiscardAndCloseIgnoringErrors(resp.Body)
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
