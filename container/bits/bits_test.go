package bits

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
