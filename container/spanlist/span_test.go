package spanlist_test

import (
	"testing"

	"github.com/richardwilkes/toolbox/check"
	"github.com/richardwilkes/torrent/container/spanlist"
)

func TestSpan(t *testing.T) {
	var list spanlist.SpanList

	// Insert
	hadOverlap := list.Insert(&spanlist.Span{Start: 15, Length: 20})
	check.Equal(t, 1, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 15, Length: 20}, list.Spans[0])
	check.False(t, hadOverlap)

	// Insert before
	hadOverlap = list.Insert(&spanlist.Span{Start: 0, Length: 5})
	check.Equal(t, 2, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 15, Length: 20}, list.Spans[1])
	check.False(t, hadOverlap)

	// Insert after
	hadOverlap = list.Insert(&spanlist.Span{Start: 50, Length: 10})
	check.Equal(t, 3, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 15, Length: 20}, list.Spans[1])
	check.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	check.False(t, hadOverlap)

	// Insert abut front
	hadOverlap = list.Insert(&spanlist.Span{Start: 12, Length: 3})
	check.Equal(t, 3, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 12, Length: 23}, list.Spans[1])
	check.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	check.False(t, hadOverlap)

	// Insert abut end
	hadOverlap = list.Insert(&spanlist.Span{Start: 35, Length: 3})
	check.Equal(t, 3, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 12, Length: 26}, list.Spans[1])
	check.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	check.False(t, hadOverlap)

	// Insert overlap front
	hadOverlap = list.Insert(&spanlist.Span{Start: 10, Length: 3})
	check.Equal(t, 3, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 10, Length: 28}, list.Spans[1])
	check.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	check.True(t, hadOverlap)

	// Insert overlap end
	hadOverlap = list.Insert(&spanlist.Span{Start: 28, Length: 15})
	check.Equal(t, 3, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 10, Length: 33}, list.Spans[1])
	check.Equal(t, spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	check.True(t, hadOverlap)

	// Insert overlap two ranges
	hadOverlap = list.Insert(&spanlist.Span{Start: 28, Length: 30})
	check.Equal(t, 2, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 10, Length: 50}, list.Spans[1])
	check.True(t, hadOverlap)

	// Insert after
	hadOverlap = list.Insert(&spanlist.Span{Start: 65, Length: 10})
	check.Equal(t, 3, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	check.Equal(t, spanlist.Span{Start: 10, Length: 50}, list.Spans[1])
	check.Equal(t, spanlist.Span{Start: 65, Length: 10}, list.Spans[2])
	check.False(t, hadOverlap)

	// Insert overlap three ranges
	hadOverlap = list.Insert(&spanlist.Span{Start: 0, Length: 65})
	check.Equal(t, 1, len(list.Spans))
	check.Equal(t, spanlist.Span{Start: 0, Length: 75}, list.Spans[0])
	check.True(t, hadOverlap)
}
