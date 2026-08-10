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
	"bytes"
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

// TestByteLength verifies the storage size reported for a given number of bits, which peer.go relies on to validate the
// length of an incoming bit field message.
func TestByteLength(t *testing.T) {
	c := check.New(t)
	for _, one := range []struct {
		bits  int
		bytes int
	}{
		{bits: -8, bytes: 0},
		{bits: -1, bytes: 0},
		{bits: 0, bytes: 0},
		{bits: 1, bytes: 1},
		{bits: 7, bytes: 1},
		{bits: 8, bytes: 1},
		{bits: 9, bytes: 2},
		{bits: 15, bytes: 2},
		{bits: 16, bytes: 2},
		{bits: 17, bytes: 3},
		{bits: 1000, bytes: 125},
		{bits: 1001, bytes: 126},
	} {
		bm := New(one.bits)
		c.Equal(one.bytes, bm.ByteLength(), "%d bits", one.bits)
		c.Equal(one.bytes, len(bm.data), "%d bits", one.bits)
		c.Equal(max(one.bits, 0), bm.Length(), "%d bits", one.bits)
		c.Equal(one.bytes, bm.Clone().ByteLength(), "%d bits", one.bits)

		// Filling the storage doesn't change how much of it there is
		bm.SetBytes(bytes.Repeat([]byte{0xFF}, one.bytes))
		c.Equal(one.bytes, bm.ByteLength(), "%d bits", one.bits)
	}
}

// TestAnySet verifies both outcomes of AnySet, including for bits that live outside the first byte.
func TestAnySet(t *testing.T) {
	c := check.New(t)
	c.False(New(0).AnySet())
	c.False(New(20).AnySet())

	bm := New(20)
	for _, i := range []int{0, 7, 8, 15, 19} {
		bm.Set(i)
		c.True(bm.AnySet(), "bit %d", i)
		bm.Unset(i)
		c.False(bm.AnySet(), "bit %d", i)
	}

	// Out of range indexes never register
	bm.Set(-1)
	bm.Set(20)
	bm.Set(1000)
	c.False(bm.AnySet())

	// Only bits within the logical size count, since SetBytes discards the spare ones
	bm.SetBytes([]byte{0, 0, 0x0F})
	c.False(bm.AnySet())
	bm.SetBytes([]byte{0, 0, 0x10})
	c.True(bm.AnySet())
}

// TestClone verifies the clone is an independent copy rather than a second view onto the same storage.
func TestClone(t *testing.T) {
	c := check.New(t)
	bm := New(20)
	bm.Set(3)
	bm.Set(19)
	clone := bm.Clone()
	c.Equal(bm.Length(), clone.Length())
	c.Equal(bm.ByteLength(), clone.ByteLength())
	c.Equal(bm.data, clone.data)

	// Mutating the clone must leave the original alone...
	clone.Set(10)
	clone.Unset(3)
	c.True(clone.IsSet(10))
	c.False(bm.IsSet(10))
	c.False(clone.IsSet(3))
	c.True(bm.IsSet(3))

	// ...and mutating the original must leave the clone alone
	bm.Set(0)
	bm.Unset(19)
	c.True(bm.IsSet(0))
	c.False(clone.IsSet(0))
	c.False(bm.IsSet(19))
	c.True(clone.IsSet(19))

	// Replacing the whole of one's storage must not disturb the other
	clone.SetBytes([]byte{0xFF, 0xFF, 0xFF})
	c.Equal(-1, clone.NextUnset(0))
	c.Equal(1, bm.NextUnset(0))

	// An empty set of bits clones without incident
	empty := New(0).Clone()
	c.Equal(0, empty.Length())
	c.Equal(0, empty.ByteLength())
	c.False(empty.AnySet())
}

// TestSetBytesWithOversizedBuffer verifies that a buffer holding more than the backing storage can accept is truncated
// rather than growing the storage, since the buffer comes from a remote peer.
func TestSetBytesWithOversizedBuffer(t *testing.T) {
	c := check.New(t)
	bm := New(4)
	bm.SetBytes([]byte{0xF0, 0xFF, 0xFF})
	c.Equal(1, bm.ByteLength())
	c.Equal(4, bm.Length())
	c.Equal(uint8(0xF0), bm.data[0])
	c.Equal(-1, bm.NextUnset(0))

	// An empty set of bits accepts anything and retains nothing
	bm = New(0)
	bm.SetBytes([]byte{0xFF, 0xFF})
	c.Equal(0, bm.ByteLength())
	c.False(bm.AnySet())
}
