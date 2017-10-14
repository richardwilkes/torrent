package spanlist_test

import (
	"testing"

	"github.com/richardwilkes/torrent/container/spanlist"
	"github.com/stretchr/testify/assert"
)

func TestSpan(t *testing.T) {
	var list spanlist.SpanList

	// Insert
	hadOverlap := list.Insert(&spanlist.Span{Start: 15, Length: 20})
	assert.Equal(t, 1, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 15, Length: 20}, list.Spans[0])
	assert.False(t, hadOverlap)

	// Insert before
	hadOverlap = list.Insert(&spanlist.Span{Start: 0, Length: 5})
	assert.Equal(t, 2, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 15, Length: 20}, list.Spans[1])
	assert.False(t, hadOverlap)

	// Insert after
	hadOverlap = list.Insert(&spanlist.Span{Start: 50, Length: 10})
	assert.Equal(t, 3, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 15, Length: 20}, list.Spans[1])
	assert.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	assert.False(t, hadOverlap)

	// Insert abut front
	hadOverlap = list.Insert(&spanlist.Span{Start: 12, Length: 3})
	assert.Equal(t, 3, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 12, Length: 23}, list.Spans[1])
	assert.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	assert.False(t, hadOverlap)

	// Insert abut end
	hadOverlap = list.Insert(&spanlist.Span{Start: 35, Length: 3})
	assert.Equal(t, 3, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 12, Length: 26}, list.Spans[1])
	assert.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	assert.False(t, hadOverlap)

	// Insert overlap front
	hadOverlap = list.Insert(&spanlist.Span{Start: 10, Length: 3})
	assert.Equal(t, 3, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 10, Length: 28}, list.Spans[1])
	assert.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	assert.True(t, hadOverlap)

	// Insert overlap end
	hadOverlap = list.Insert(&spanlist.Span{Start: 28, Length: 15})
	assert.Equal(t, 3, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 10, Length: 33}, list.Spans[1])
	assert.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	assert.True(t, hadOverlap)

	// Insert overlap two ranges
	hadOverlap = list.Insert(&spanlist.Span{Start: 28, Length: 30})
	assert.Equal(t, 2, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 10, Length: 50}, list.Spans[1])
	assert.True(t, hadOverlap)

	// Insert after
	hadOverlap = list.Insert(&spanlist.Span{Start: 65, Length: 10})
	assert.Equal(t, 3, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	assert.Equal(t, spanlist.Span{Start: 10, Length: 50}, list.Spans[1])
	assert.Equal(t, spanlist.Span{Start: 65, Length: 10}, list.Spans[2])
	assert.False(t, hadOverlap)

	// Insert overlap three ranges
	hadOverlap = list.Insert(&spanlist.Span{Start: 0, Length: 65})
	assert.Equal(t, 1, len(list.Spans))
	assert.Equal(t, spanlist.Span{Start: 0, Length: 75}, list.Spans[0])
	assert.True(t, hadOverlap)
}
