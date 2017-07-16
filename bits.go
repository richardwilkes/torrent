package torrent

type bits struct {
	data []byte
	size int
}

func newBits(numberOfBits int) *bits {
	if numberOfBits < 0 {
		numberOfBits = 0
	}
	size := numberOfBits / 8
	if numberOfBits%8 != 0 {
		size++
	}
	return &bits{
		data: make([]byte, size),
		size: numberOfBits,
	}
}

func (b *bits) Clone() *bits {
	c := &bits{
		data: make([]byte, len(b.data)),
		size: b.size,
	}
	copy(c.data, b.data)
	return c
}

func (b *bits) AnySet() bool {
	for _, one := range b.data {
		if one != 0 {
			return true
		}
	}
	return false
}

func (b *bits) IsSet(index int) bool {
	if index < 0 || index >= b.size {
		return false
	}
	return b.data[index/8]&(1<<uint(7-(index%8))) != 0
}

func (b *bits) Set(index int) {
	if index >= 0 && index < b.size {
		b.data[index/8] |= 1 << uint(7-(index%8))
	}
}

func (b *bits) Unset(index int) {
	if index >= 0 && index < b.size {
		b.data[index/8] &^= 1 << uint(7-(index%8))
	}
}

func (b *bits) NextSet(from int) int {
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

func (b *bits) NextUnset(from int) int {
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
