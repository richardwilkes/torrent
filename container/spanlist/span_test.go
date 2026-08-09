// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

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

func TestSpanInsertIntoGap(t *testing.T) {
	c := check.New(t)

	var list spanlist.SpanList
	c.False(list.Insert(&spanlist.Span{Start: 0, Length: 10}))
	c.False(list.Insert(&spanlist.Span{Start: 40, Length: 10}))
	c.False(list.Insert(&spanlist.Span{Start: 80, Length: 10}))

	// Insert into the gap between the first and second spans; must land at index 1, not index 0
	c.False(list.Insert(&spanlist.Span{Start: 20, Length: 10}))
	c.Equal([]spanlist.Span{
		{Start: 0, Length: 10},
		{Start: 20, Length: 10},
		{Start: 40, Length: 10},
		{Start: 80, Length: 10},
	}, list.Spans)

	// Insert into the gap just before the last span; must land at index 4
	c.False(list.Insert(&spanlist.Span{Start: 60, Length: 5}))
	c.Equal([]spanlist.Span{
		{Start: 0, Length: 10},
		{Start: 20, Length: 10},
		{Start: 40, Length: 10},
		{Start: 60, Length: 5},
		{Start: 80, Length: 10},
	}, list.Spans)

	// Insert into a gap ahead of everything; must land at index 0
	c.False(list.Insert(&spanlist.Span{Start: -20, Length: 10}))
	c.Equal(spanlist.Span{Start: -20, Length: 10}, list.Spans[0])
	c.Equal(6, len(list.Spans))
}

func TestSpanInsertOutOfOrder(t *testing.T) {
	c := check.New(t)

	// Mimic chunks of a piece arriving out of order. Each chunk is 10 bytes long and the piece is 5 chunks long. Only
	// once all 5 have arrived should the list collapse to a single span covering the whole piece.
	for _, order := range [][]int{
		{0, 4, 2, 3, 1},
		{4, 3, 2, 1, 0},
		{2, 0, 4, 1, 3},
		{1, 3, 0, 2, 4},
		{3, 1, 4, 0, 2},
	} {
		var list spanlist.SpanList
		for i, chunk := range order {
			c.False(list.Insert(&spanlist.Span{Start: chunk * 10, Length: 10}), "order %v, chunk %d", order, chunk)
			// The list must always remain sorted, non-overlapping and free of gaps that weren't inserted
			covered := 0
			for j, span := range list.Spans {
				if j > 0 {
					prev := list.Spans[j-1]
					c.True(prev.Start+prev.Length < span.Start, "order %v, chunk %d, span %d", order, chunk, j)
				}
				covered += span.Length
			}
			c.Equal((i+1)*10, covered, "order %v, chunk %d", order, chunk)
		}
		c.Equal([]spanlist.Span{{Start: 0, Length: 50}}, list.Spans, "order %v", order)
	}
}
