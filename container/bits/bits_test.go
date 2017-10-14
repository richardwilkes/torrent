package bits

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBits(t *testing.T) {
	bm := New(8)
	assert.Equal(t, 1, len(bm.data))
	bm.Set(7)
	assert.True(t, bm.IsSet(7))
	assert.False(t, bm.IsSet(6))
	assert.False(t, bm.IsSet(100))
	assert.Equal(t, uint8(1), bm.data[0])
	bm.Set(0)
	assert.Equal(t, uint8(129), bm.data[0])
	bm = New(22)
	assert.Equal(t, 3, len(bm.data))
	bm.Set(22)
	for _, b := range bm.data {
		assert.Equal(t, uint8(0), b)
	}
	bm.Set(-1)
	for _, b := range bm.data {
		assert.Equal(t, uint8(0), b)
	}
	assert.Equal(t, -1, bm.NextSet(0))
	bm.Set(21)
	assert.Equal(t, 21, bm.NextSet(0))
	bm.Set(4)
	assert.Equal(t, 4, bm.NextSet(0))
	bm.Set(14)
	assert.Equal(t, 4, bm.NextSet(0))
	bm.Unset(4)
	assert.Equal(t, 14, bm.NextSet(0))
	assert.Equal(t, 21, bm.NextSet(15))
	assert.Equal(t, 0, bm.NextUnset(0))
	assert.Equal(t, 15, bm.NextUnset(14))
	assert.Equal(t, -1, bm.NextUnset(21))
}
