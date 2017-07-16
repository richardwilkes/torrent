package torrent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpan(t *testing.T) {
	var hadOverlap bool
	list := make([]span, 0)

	// Insert
	list, hadOverlap = span{start: 15, length: 20}.insertInto(list)
	assert.Equal(t, 1, len(list))
	assert.Equal(t, span{start: 15, length: 20}, list[0])
	assert.False(t, hadOverlap)

	// Insert before
	list, hadOverlap = span{start: 0, length: 5}.insertInto(list)
	assert.Equal(t, 2, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 15, length: 20}, list[1])
	assert.False(t, hadOverlap)

	// Insert after
	list, hadOverlap = span{start: 50, length: 10}.insertInto(list)
	assert.Equal(t, 3, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 15, length: 20}, list[1])
	assert.Equal(t, span{start: 50, length: 10}, list[2])
	assert.False(t, hadOverlap)

	// Insert abut front
	list, hadOverlap = span{start: 12, length: 3}.insertInto(list)
	assert.Equal(t, 3, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 12, length: 23}, list[1])
	assert.Equal(t, span{start: 50, length: 10}, list[2])
	assert.False(t, hadOverlap)

	// Insert abut end
	list, hadOverlap = span{start: 35, length: 3}.insertInto(list)
	assert.Equal(t, 3, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 12, length: 26}, list[1])
	assert.Equal(t, span{start: 50, length: 10}, list[2])
	assert.False(t, hadOverlap)

	// Insert overlap front
	list, hadOverlap = span{start: 10, length: 3}.insertInto(list)
	assert.Equal(t, 3, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 10, length: 28}, list[1])
	assert.Equal(t, span{start: 50, length: 10}, list[2])
	assert.True(t, hadOverlap)

	// Insert overlap end
	list, hadOverlap = span{start: 28, length: 15}.insertInto(list)
	assert.Equal(t, 3, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 10, length: 33}, list[1])
	assert.Equal(t, span{start: 50, length: 10}, list[2])
	assert.True(t, hadOverlap)

	// Insert overlap two ranges
	list, hadOverlap = span{start: 28, length: 30}.insertInto(list)
	assert.Equal(t, 2, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 10, length: 50}, list[1])
	assert.True(t, hadOverlap)

	// Insert after
	list, hadOverlap = span{start: 65, length: 10}.insertInto(list)
	assert.Equal(t, 3, len(list))
	assert.Equal(t, span{start: 0, length: 5}, list[0])
	assert.Equal(t, span{start: 10, length: 50}, list[1])
	assert.Equal(t, span{start: 65, length: 10}, list[2])
	assert.False(t, hadOverlap)

	// Insert overlap three ranges
	list, hadOverlap = span{start: 0, length: 65}.insertInto(list)
	assert.Equal(t, 1, len(list))
	assert.Equal(t, span{start: 0, length: 75}, list[0])
	assert.True(t, hadOverlap)
}
