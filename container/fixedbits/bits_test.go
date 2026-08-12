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

// TestSpareBitsSet verifies that a buffer carrying bits past the end of the set can be told apart from one that
// doesn't, which is what a caller obliged to refuse the first — BEP 3 requires the trailing bits of a bit field to be
// zero and has a downloader drop a peer that sets them — has to ask before SetBytes discards the evidence.
func TestSpareBitsSet(t *testing.T) {
	c := check.New(t)

	// 12 bits requires 2 bytes of storage, leaving 4 spare bits
	bm := New(12)
	c.False(bm.SpareBitsSet([]byte{0xFF, 0xF0}), "every bit the set holds may be set")
	c.False(bm.SpareBitsSet([]byte{0x00, 0x00}))
	c.True(bm.SpareBitsSet([]byte{0x00, 0x08}), "the first spare bit must be noticed")
	c.True(bm.SpareBitsSet([]byte{0xFF, 0xFF}))
	c.True(bm.SpareBitsSet([]byte{0x00, 0x01}), "the last spare bit must be noticed")

	// A size that fills its storage exactly has no spare bits at all
	bm = New(16)
	c.False(bm.SpareBitsSet([]byte{0xFF, 0xFF}))

	// Whole bytes past the storage are spare too, however the size rounds up
	bm = New(4)
	c.False(bm.SpareBitsSet([]byte{0xF0}))
	c.True(bm.SpareBitsSet([]byte{0xF0, 0x00, 0x01}))
	c.False(bm.SpareBitsSet([]byte{0xF0, 0x00, 0x00}))

	// A buffer that stops short of the storage carries no spare bits, since none of it reaches them
	bm = New(20)
	c.False(bm.SpareBitsSet([]byte{0xFF}))
	c.False(bm.SpareBitsSet(nil))

	// A set holding nothing has nothing but spare bits
	bm = New(0)
	c.False(bm.SpareBitsSet(nil))
	c.False(bm.SpareBitsSet([]byte{0x00}))
	c.True(bm.SpareBitsSet([]byte{0x01}))

	// What it reports is exactly what SetBytes would throw away
	for _, buffer := range [][]byte{{0xFF, 0xFF}, {0xFF, 0xF0}, {0x00, 0x08}, {0x12, 0x34}} {
		bm = New(12)
		reported := bm.SpareBitsSet(buffer)
		bm.SetBytes(buffer)
		c.Equal(reported, !bytes.Equal(buffer, bm.data), "buffer %v", buffer)
	}
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

// TestFirstAvailableExcludesEitherSet verifies the documented contract: an index has to be clear in both 'downloading'
// and 'have' to be available, so being set in just one of them is enough to rule it out. A piece already being
// downloaded from some other peer is not something to hand out again.
func TestFirstAvailableExcludesEitherSet(t *testing.T) {
	c := check.New(t)
	const pieceCount = 3
	for _, one := range []struct {
		exclude  func(downloading, have *Bits)
		name     string
		expected int
	}{
		{
			name:     "nothing excluded",
			exclude:  func(_, _ *Bits) {},
			expected: 0,
		},
		{
			name:     "downloading only",
			exclude:  func(downloading, _ *Bits) { downloading.Set(0) },
			expected: 1,
		},
		{
			name:     "have only",
			exclude:  func(_, have *Bits) { have.Set(0) },
			expected: 1,
		},
		{
			name:     "both",
			exclude:  func(downloading, have *Bits) { downloading.Set(0); have.Set(0) },
			expected: 1,
		},
		{
			name: "each index excluded by a different set",
			exclude: func(downloading, have *Bits) {
				downloading.Set(0)
				have.Set(1)
				downloading.Set(2)
			},
			expected: -1,
		},
	} {
		has := New(pieceCount)
		for i := range pieceCount {
			has.Set(i)
		}
		downloading := New(pieceCount)
		have := New(pieceCount)
		one.exclude(downloading, have)
		c.Equal(one.expected, FirstAvailable(has, downloading, have), one.name)
	}
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

// TestFirstMissing verifies that an index only has to be one we don't have, whether or not anything has claimed it,
// which is what deciding our interest in a peer turns on.
func TestFirstMissing(t *testing.T) {
	c := check.New(t)
	const pieceCount = 5
	has := New(pieceCount)
	have := New(pieceCount)
	c.Equal(-1, FirstMissing(has, have))

	has.SetBytes([]byte{0xFF})
	c.Equal(0, FirstMissing(has, have))
	have.Set(0)
	c.Equal(1, FirstMissing(has, have))

	// Every real piece accounted for leaves nothing, even though the peer set the spare bits as well
	for i := range pieceCount {
		have.Set(i)
	}
	c.Equal(-1, FirstMissing(has, have))

	// The smaller of the two limits the result, since anything beyond that isn't known to both
	shortHave := New(3)
	has.data[0] = 0
	has.Set(3)
	c.Equal(-1, FirstMissing(has, shortHave))
	has.Set(2)
	c.Equal(2, FirstMissing(has, shortHave))
}

// TestFirstAvailableAndFirstMissingMatchADirectSearch verifies the byte at a time combination against the plain
// definition of what the two answer, for every combination of a handful of bits and for sets that differ in size. The
// answer has to be exactly what a bit by bit search of the whole would have given, including which of the sets bounds
// the search and what happens when the last byte reaches past that bound.
func TestFirstAvailableAndFirstMissingMatchADirectSearch(t *testing.T) {
	c := check.New(t)
	// Short of a full byte, so that the spare bits of the last byte are in play, and small enough to try every
	// combination of the three sets exhaustively
	const width = 5
	for _, sizes := range [][3]int{
		{width, width, width},
		{width, width - 2, width},
		{width - 2, width, width},
		{width, width, width - 2},
		{0, width, width},
		{width, width, width + 3},
		{width + 8, width, width + 4},
	} {
		for hasBits := range 1 << width {
			for downloadingBits := range 1 << width {
				for haveBits := range 1 << width {
					has := bitsFrom(sizes[0], hasBits)
					downloading := bitsFrom(sizes[1], downloadingBits)
					have := bitsFrom(sizes[2], haveBits)
					c.Equal(firstAvailableBySearch(has, downloading, have), FirstAvailable(has, downloading, have),
						"sizes %v, has %05b, downloading %05b, have %05b", sizes, hasBits, downloadingBits, haveBits)
					c.Equal(firstMissingBySearch(has, have), FirstMissing(has, have),
						"sizes %v, has %05b, have %05b", sizes, hasBits, haveBits)
				}
			}
		}
	}
}

// bitsFrom builds a set of the given size whose bits are taken from the low order end of 'pattern', with the first
// index taking the lowest order bit. Indexes the set is too small to hold are dropped, exactly as Set does.
func bitsFrom(size, pattern int) *Bits {
	b := New(size)
	for i := 0; pattern != 0; i++ {
		if pattern&1 != 0 {
			b.Set(i)
		}
		pattern >>= 1
	}
	return b
}

// firstAvailableBySearch answers the question FirstAvailable does, by looking at one index at a time.
func firstAvailableBySearch(has, downloading, have *Bits) int {
	for i := range min(has.size, downloading.size, have.size) {
		if has.IsSet(i) && !downloading.IsSet(i) && !have.IsSet(i) {
			return i
		}
	}
	return -1
}

// firstMissingBySearch answers the question FirstMissing does, by looking at one index at a time.
func firstMissingBySearch(has, have *Bits) int {
	for i := range min(has.size, have.size) {
		if has.IsSet(i) && !have.IsSet(i) {
			return i
		}
	}
	return -1
}

// TestFirstAvailableAndFirstMissingAllocateNothing verifies that the sets are combined in place rather than into a copy
// of them that is then scanned. A peer drives both from every 'have' message it sends, and FirstAvailable runs under
// the tracker's write lock, so at the largest piece count the library allows a copy would cost a 256KB allocation and a
// full scan of it per message, with every other peer's piece selection waiting on the lock while it happens.
func TestFirstAvailableAndFirstMissingAllocateNothing(t *testing.T) {
	c := check.New(t)
	const pieceCount = 1 << 16
	has := New(pieceCount)
	downloading := New(pieceCount)
	have := New(pieceCount)
	for i := range pieceCount {
		has.Set(i)
		have.Set(i)
		downloading.Set(i)
	}
	// Nothing to find, which is the case that has to look at the whole of the storage before it can say so
	c.Equal(-1, FirstAvailable(has, downloading, have))
	c.Equal(-1, FirstMissing(has, have))
	c.Equal(float64(0), testing.AllocsPerRun(10, func() {
		FirstAvailable(has, downloading, have)
		FirstMissing(has, have)
	}))
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

// TestBytes verifies that the backing storage can be handed out, which is what a bit field message is built from, and
// that what comes back is a copy rather than the storage itself.
func TestBytes(t *testing.T) {
	c := check.New(t)
	c.Equal([]byte{}, New(0).Bytes())

	bm := New(12)
	c.Equal([]byte{0, 0}, bm.Bytes())
	bm.Set(0)
	bm.Set(11)
	c.Equal([]byte{0x80, 0x10}, bm.Bytes())

	// Altering what was returned must not alter the bits it came from
	buffer := bm.Bytes()
	buffer[0] = 0xFF
	c.Equal([]byte{0x80, 0x10}, bm.Bytes())
	c.False(bm.IsSet(1))
}
