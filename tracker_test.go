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
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
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
