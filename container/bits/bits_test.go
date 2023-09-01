package bits

import (
	"testing"

	"github.com/richardwilkes/toolbox/check"
)

func TestBits(t *testing.T) {
	bm := New(8)
	check.Equal(t, 1, len(bm.data))
	bm.Set(7)
	check.True(t, bm.IsSet(7))
	check.False(t, bm.IsSet(6))
	check.False(t, bm.IsSet(100))
	check.Equal(t, uint8(1), bm.data[0])
	bm.Set(0)
	check.Equal(t, uint8(129), bm.data[0])
	bm = New(22)
	check.Equal(t, 3, len(bm.data))
	bm.Set(22)
	for _, b := range bm.data {
		check.Equal(t, uint8(0), b)
	}
	bm.Set(-1)
	for _, b := range bm.data {
		check.Equal(t, uint8(0), b)
	}
	check.Equal(t, -1, bm.NextSet(0))
	bm.Set(21)
	check.Equal(t, 21, bm.NextSet(0))
	bm.Set(4)
	check.Equal(t, 4, bm.NextSet(0))
	bm.Set(14)
	check.Equal(t, 4, bm.NextSet(0))
	bm.Unset(4)
	check.Equal(t, 14, bm.NextSet(0))
	check.Equal(t, 21, bm.NextSet(15))
	check.Equal(t, 0, bm.NextUnset(0))
	check.Equal(t, 15, bm.NextUnset(14))
	check.Equal(t, -1, bm.NextUnset(21))
}
