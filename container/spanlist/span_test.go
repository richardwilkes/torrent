package spanlist_test

import (
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent/container/spanlist"
)

func TestSpan(t *testing.T) {
	c := check.New(t)

	var list spanlist.SpanList

	// Insert
	hadOverlap := list.Insert(&spanlist.Span{Start: 15, Length: 20})
	c.Equal(1, len(list.Spans))
	c.Equal(spanlist.Span{Start: 15, Length: 20}, list.Spans[0])
	c.False(hadOverlap)

	// Insert before
	hadOverlap = list.Insert(&spanlist.Span{Start: 0, Length: 5})
	c.Equal(2, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 15, Length: 20}, list.Spans[1])
	c.False(hadOverlap)

	// Insert after
	hadOverlap = list.Insert(&spanlist.Span{Start: 50, Length: 10})
	c.Equal(3, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 15, Length: 20}, list.Spans[1])
	c.Equal(spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	c.False(hadOverlap)

	// Insert abut front
	hadOverlap = list.Insert(&spanlist.Span{Start: 12, Length: 3})
	c.Equal(3, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 12, Length: 23}, list.Spans[1])
	c.Equal(spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	c.False(hadOverlap)

	// Insert abut end
	hadOverlap = list.Insert(&spanlist.Span{Start: 35, Length: 3})
	c.Equal(3, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 12, Length: 26}, list.Spans[1])
	c.Equal(spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	c.False(hadOverlap)

	// Insert overlap front
	hadOverlap = list.Insert(&spanlist.Span{Start: 10, Length: 3})
	c.Equal(3, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 10, Length: 28}, list.Spans[1])
	c.Equal(spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	c.True(hadOverlap)

	// Insert overlap end
	hadOverlap = list.Insert(&spanlist.Span{Start: 28, Length: 15})
	c.Equal(3, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 10, Length: 33}, list.Spans[1])
	c.Equal(spanlist.Span{Start: 50, Length: 10}, list.Spans[2])
	c.True(hadOverlap)

	// Insert overlap two ranges
	hadOverlap = list.Insert(&spanlist.Span{Start: 28, Length: 30})
	c.Equal(2, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 10, Length: 50}, list.Spans[1])
	c.True(hadOverlap)

	// Insert after
	hadOverlap = list.Insert(&spanlist.Span{Start: 65, Length: 10})
	c.Equal(3, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 5}, list.Spans[0])
	c.Equal(spanlist.Span{Start: 10, Length: 50}, list.Spans[1])
	c.Equal(spanlist.Span{Start: 65, Length: 10}, list.Spans[2])
	c.False(hadOverlap)

	// Insert overlap three ranges
	hadOverlap = list.Insert(&spanlist.Span{Start: 0, Length: 65})
	c.Equal(1, len(list.Spans))
	c.Equal(spanlist.Span{Start: 0, Length: 75}, list.Spans[0])
	c.True(hadOverlap)
}
