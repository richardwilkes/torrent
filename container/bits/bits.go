package bits

import (
	"github.com/richardwilkes/xmath"
)

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
	size := numberOfBits / 8
	if numberOfBits%8 != 0 {
		size++
	}
	return &Bits{
		data: make([]byte, size),
		size: numberOfBits,
	}
}

// FirstAvailable returns the first index set in 'has' and is not set in both
// 'downloading' and 'have', or -1 if no such index exists.
func FirstAvailable(has, downloading, have *Bits) int {
	max := xmath.MinInt(xmath.MinInt(len(has.data), len(downloading.data)), len(have.data))
	avail := New(max * 8)
	for i := range avail.data {
		avail.data[i] = has.data[i] &^ downloading.data[i] &^ have.data[i]
	}
	return avail.NextSet(0)
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
// remaining bytes will remain as-is.
func (b *Bits) SetBytes(buffer []byte) {
	copy(b.data, buffer)
}

// ByteLength returns the number of bytes required to hold the data.
func (b *Bits) ByteLength() int {
	return len(b.data)
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
	if from >= 0 && from < b.size {
		i := from / 8
		one := b.data[i]
		if one != 0 {
			for j := 7 - (from % 8); j >= 0; j-- {
				if one&(1<<uint(j)) != 0 {
					i = i*8 + 7 - j
					if i < b.size {
						return i
					}
					return -1
				}
			}
			i++
			if i >= len(b.data) {
				return -1
			}
			one = b.data[i]
		}
		for {
			if one != 0 {
				for j := 7; j > 0; j-- {
					if one&(1<<uint(j)) != 0 {
						i = i*8 + 7 - j
						if i < b.size {
							return i
						}
						return -1
					}
				}
				i = i*8 + 7
				if i < b.size {
					return i
				}
				return -1
			}
			i++
			if i >= len(b.data) {
				break
			}
			one = b.data[i]
		}
	}
	return -1
}

// NextUnset returns the index of the next unset bit, starting at 'from'.
// Returns -1 if no bits are unset from 'from' through the end of the bits.
func (b *Bits) NextUnset(from int) int {
	if from >= 0 && from < b.size {
		i := from / 8
		one := b.data[i]
		if one != 255 {
			for j := 7 - (from % 8); j >= 0; j-- {
				if one&(1<<uint(j)) == 0 {
					i = i*8 + 7 - j
					if i < b.size {
						return i
					}
					return -1
				}
			}
			i++
			if i >= len(b.data) {
				return -1
			}
			one = b.data[i]
		}
		for {
			if one != 255 {
				for j := 7; j > 0; j-- {
					if one&(1<<uint(j)) == 0 {
						i = i*8 + 7 - j
						if i < b.size {
							return i
						}
						return -1
					}
				}
				i = i*8 + 7
				if i < b.size {
					return i
				}
				return -1
			}
			i++
			if i >= len(b.data) {
				break
			}
			one = b.data[i]
		}
	}
	return -1
}
