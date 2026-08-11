// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package fixedbits

import "math/bits"

// Bits holds a fixed-size collection of bits.
type Bits struct {
	data []byte
	size int
}

// New creates a new set of bits, all unset.
func New(numberOfBits int) *Bits {
	if numberOfBits < 0 {
		numberOfBits = 0
	}
	return &Bits{
		data: make([]byte, byteLengthOf(numberOfBits)),
		size: numberOfBits,
	}
}

// byteLengthOf returns the number of bytes needed to hold the given number of bits.
func byteLengthOf(numberOfBits int) int {
	return (numberOfBits + 7) / 8
}

// FirstAvailable returns the first index that is set in 'has' and is set in
// neither 'downloading' nor 'have', or -1 if no such index exists.
//
// The three are combined a byte at a time and the walk ends at the first byte
// with anything in it, rather than the whole combination being built up front
// and scanned afterwards. A peer drives this from every 'have' message it
// sends, by way of the piece selection that runs under the tracker's write
// lock, so a torrent with the largest piece count the library allows would
// otherwise pay a 256KB allocation and a full scan of it for each of them
// while every other peer waits for that lock.
func FirstAvailable(has, downloading, have *Bits) int {
	size := min(has.size, downloading.size, have.size)
	for i := range byteLengthOf(size) {
		if one := has.data[i] &^ downloading.data[i] &^ have.data[i]; one != 0 {
			return firstSetIndex(i, one, size)
		}
	}
	return -1
}

// FirstMissing returns the first index that is set in 'has' and is not set in
// 'have', or -1 if no such index exists. Unlike FirstAvailable, an index that
// something else has already claimed still counts. Combined a byte at a time
// and stopped at the first hit, for the reasons FirstAvailable gives.
func FirstMissing(has, have *Bits) int {
	size := min(has.size, have.size)
	for i := range byteLengthOf(size) {
		if one := has.data[i] &^ have.data[i]; one != 0 {
			return firstSetIndex(i, one, size)
		}
	}
	return -1
}

// firstSetIndex returns the index of the highest order bit set in a non-zero
// byte at the given position, or -1 if that index is beyond 'size'. Nothing
// further along can be within it either, since every later bit has a higher
// index, so the whole scan is over at that point. The bits of the byte the
// combination didn't cover are what puts an index out of range: only the
// narrowest of the sets being combined has had its spare bits cleared to the
// size in use here.
func firstSetIndex(byteIndex int, b byte, size int) int {
	if index := byteIndex*8 + bits.LeadingZeros8(b); index < size {
		return index
	}
	return -1
}

// Clone the bits into a fresh copy.
func (b *Bits) Clone() *Bits {
	c := &Bits{
		data: make([]byte, len(b.data)),
		size: b.size,
	}
	copy(c.data, b.data)
	return c
}

// SetBytes sets the bytes in the buffer into the backing storage for this
// bits object. If the buffer is shorter than the backing storage, the
// remaining bytes will remain as-is. Any bits in the buffer beyond the number
// of bits this object holds are discarded.
func (b *Bits) SetBytes(buffer []byte) {
	copy(b.data, buffer)
	b.clearSpareBits()
}

// clearSpareBits clears any bits in the backing storage that are beyond the
// number of bits this object holds, since they aren't valid and would
// otherwise be seen as set by the methods that scan the backing storage.
func (b *Bits) clearSpareBits() {
	if spare := len(b.data)*8 - b.size; spare > 0 {
		b.data[len(b.data)-1] &= 0xFF << uint(spare)
	}
}

// ByteLength returns the number of bytes required to hold the data.
func (b *Bits) ByteLength() int {
	return len(b.data)
}

// Bytes returns a copy of the backing storage for these bits.
func (b *Bits) Bytes() []byte {
	buffer := make([]byte, len(b.data))
	copy(buffer, b.data)
	return buffer
}

// Length returns the number of bits contained.
func (b *Bits) Length() int {
	return b.size
}

// AnySet returns true if any bit is set.
func (b *Bits) AnySet() bool {
	for _, one := range b.data {
		if one != 0 {
			return true
		}
	}
	return false
}

// IsSet returns true if the specified index is set.
func (b *Bits) IsSet(index int) bool {
	if index < 0 || index >= b.size {
		return false
	}
	return b.data[index/8]&(1<<uint(7-(index%8))) != 0
}

// Set the specified index.
func (b *Bits) Set(index int) {
	if index >= 0 && index < b.size {
		b.data[index/8] |= 1 << uint(7-(index%8))
	}
}

// Unset the specified index.
func (b *Bits) Unset(index int) {
	if index >= 0 && index < b.size {
		b.data[index/8] &^= 1 << uint(7-(index%8))
	}
}

// NextSet returns the index of the next set bit, starting at 'from'. Returns
// -1 if no bits are set from 'from' through the end of the bits.
func (b *Bits) NextSet(from int) int {
	if from < 0 || from >= b.size {
		return -1
	}
	start := 7 - (from % 8)
	i := from / 8
	one := b.data[i]
	for {
		if one != 0 {
			for j := start; j >= 0; j-- {
				if one&(1<<uint(j)) != 0 {
					if i = i*8 + 7 - j; i < b.size {
						return i
					}
					return -1
				}
			}
			if start == 7 {
				return -1
			}
		}
		i++
		if i >= len(b.data) {
			return -1
		}
		one = b.data[i]
		start = 7
	}
}

// NextUnset returns the index of the next unset bit, starting at 'from'.
// Returns -1 if no bits are unset from 'from' through the end of the bits.
func (b *Bits) NextUnset(from int) int {
	if from < 0 || from >= b.size {
		return -1
	}
	start := 7 - (from % 8)
	i := from / 8
	one := b.data[i]
	for {
		if one != 255 {
			for j := start; j >= 0; j-- {
				if one&(1<<uint(j)) == 0 {
					if i = i*8 + 7 - j; i < b.size {
						return i
					}
					return -1
				}
			}
			if start == 7 {
				return -1
			}
		}
		i++
		if i >= len(b.data) {
			return -1
		}
		one = b.data[i]
		start = 7
	}
}
