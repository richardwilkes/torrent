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
