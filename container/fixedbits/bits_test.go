// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package fixedbits

import (
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
)

func TestBits(t *testing.T) {
	c := check.New(t)
	bm := New(8)
	c.Equal(1, len(bm.data))
	bm.Set(7)
	c.True(bm.IsSet(7))
	c.False(bm.IsSet(6))
	c.False(bm.IsSet(100))
	c.Equal(uint8(1), bm.data[0])
	bm.Set(0)
	c.Equal(uint8(129), bm.data[0])
	bm = New(22)
	c.Equal(3, len(bm.data))
	bm.Set(22)
	for _, b := range bm.data {
		c.Equal(uint8(0), b)
	}
	bm.Set(-1)
	for _, b := range bm.data {
		c.Equal(uint8(0), b)
	}
	c.Equal(-1, bm.NextSet(0))
	bm.Set(21)
	c.Equal(21, bm.NextSet(0))
	bm.Set(4)
	c.Equal(4, bm.NextSet(0))
	bm.Set(14)
	c.Equal(4, bm.NextSet(0))
	bm.Unset(4)
	c.Equal(14, bm.NextSet(0))
	c.Equal(21, bm.NextSet(15))
	c.Equal(0, bm.NextUnset(0))
	c.Equal(15, bm.NextUnset(14))
	c.Equal(-1, bm.NextUnset(21))
}

// TestSetBytesDiscardsSpareBits verifies that bits beyond the logical size, which a remote peer is free to set in the
// bit field it sends us, are not retained.
func TestSetBytesDiscardsSpareBits(t *testing.T) {
	c := check.New(t)

	// 12 bits requires 2 bytes of storage, leaving 4 spare bits
	bm := New(12)
	bm.SetBytes([]byte{0xFF, 0xFF})
	c.Equal(uint8(0xFF), bm.data[0])
	c.Equal(uint8(0xF0), bm.data[1])
	for i := range 12 {
		c.True(bm.IsSet(i), "bit %d should be set", i)
	}
	c.Equal(-1, bm.NextUnset(0))

	// Only spare bits set means nothing is set
	bm = New(4)
	bm.SetBytes([]byte{0x0F})
	c.False(bm.AnySet())
	c.Equal(-1, bm.NextSet(0))
	c.Equal(0, bm.NextUnset(0))

	// A buffer with an exact fit has no spare bits to discard
	bm = New(16)
	bm.SetBytes([]byte{0xFF, 0xFF})
	c.Equal(uint8(0xFF), bm.data[0])
	c.Equal(uint8(0xFF), bm.data[1])

	// A short buffer leaves the remaining storage as-is
	bm = New(24)
	bm.Set(20)
	bm.SetBytes([]byte{0xFF})
	c.Equal(uint8(0xFF), bm.data[0])
	c.Equal(uint8(0), bm.data[1])
	c.True(bm.IsSet(20))

	// A clone retains exactly what the original holds
	bm = New(4)
	bm.SetBytes([]byte{0xFF})
	clone := bm.Clone()
	c.Equal(bm.Length(), clone.Length())
	c.Equal(bm.data, clone.data)
	c.Equal(-1, clone.NextUnset(0))
}

func TestFirstAvailable(t *testing.T) {
	c := check.New(t)
	const pieceCount = 5
	has := New(pieceCount)
	downloading := New(pieceCount)
	have := New(pieceCount)
	c.Equal(-1, FirstAvailable(has, downloading, have))

	has.SetBytes([]byte{0xFF})
	c.Equal(0, FirstAvailable(has, downloading, have))
	have.Set(0)
	c.Equal(1, FirstAvailable(has, downloading, have))
	downloading.Set(1)
	c.Equal(2, FirstAvailable(has, downloading, have))

	// With every real piece accounted for, there is nothing left, even though the peer set the spare bits, too
	have.Set(2)
	downloading.Set(3)
	have.Set(4)
	c.Equal(-1, FirstAvailable(has, downloading, have))
}

// TestFirstAvailableLimitsToLogicalSize verifies that spare bits in the backing storage can't be returned as an
// available index, which would be beyond the number of pieces in the torrent.
func TestFirstAvailableLimitsToLogicalSize(t *testing.T) {
	c := check.New(t)
	const pieceCount = 5
	has := New(pieceCount)
	downloading := New(pieceCount)
	have := New(pieceCount)

	// Set the spare bits directly, bypassing the check made by SetBytes
	has.data[0] = 0xFF
	for i := range pieceCount {
		have.Set(i)
	}
	c.Equal(-1, FirstAvailable(has, downloading, have))

	// The smallest of the three limits the result, since anything beyond that isn't known to all of them
	shortHave := New(3)
	has.data[0] = 0
	has.Set(3)
	c.Equal(-1, FirstAvailable(has, downloading, shortHave))
	has.Set(2)
	c.Equal(2, FirstAvailable(has, downloading, shortHave))
}
