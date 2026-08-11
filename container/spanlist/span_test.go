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

// TestSpanInsertOverlapBeyondTheFirstSpan verifies that the reported overlap accounts for every span the new one is
// merged with, not just the first one it reaches. A span can abut the span it lands on and still overlap the ones that
// follow it.
func TestSpanInsertOverlapBeyondTheFirstSpan(t *testing.T) {
	c := check.New(t)

	// Abuts the first span it reaches and overlaps the second
	var list spanlist.SpanList
	c.False(list.Insert(&spanlist.Span{Start: 0, Length: 5}))
	c.False(list.Insert(&spanlist.Span{Start: 10, Length: 5}))
	c.True(list.Insert(&spanlist.Span{Start: 5, Length: 10}))
	c.Equal([]spanlist.Span{{Start: 0, Length: 15}}, list.Spans)

	// Abuts the first span it reaches and overlaps the third
	list = spanlist.SpanList{}
	c.False(list.Insert(&spanlist.Span{Start: 0, Length: 5}))
	c.False(list.Insert(&spanlist.Span{Start: 10, Length: 5}))
	c.False(list.Insert(&spanlist.Span{Start: 20, Length: 5}))
	c.True(list.Insert(&spanlist.Span{Start: 5, Length: 18}))
	c.Equal([]spanlist.Span{{Start: 0, Length: 25}}, list.Spans)

	// Filling a gap exactly abuts the spans on both sides, overlapping neither
	list = spanlist.SpanList{}
	c.False(list.Insert(&spanlist.Span{Start: 0, Length: 5}))
	c.False(list.Insert(&spanlist.Span{Start: 10, Length: 5}))
	c.False(list.Insert(&spanlist.Span{Start: 5, Length: 5}))
	c.Equal([]spanlist.Span{{Start: 0, Length: 15}}, list.Spans)

	// A span swallowed whole by an existing one overlaps it
	list = spanlist.SpanList{}
	c.False(list.Insert(&spanlist.Span{Start: 0, Length: 20}))
	c.True(list.Insert(&spanlist.Span{Start: 5, Length: 5}))
	c.Equal([]spanlist.Span{{Start: 0, Length: 20}}, list.Spans)
}

// TestSpanInsertWithoutLength verifies that a span covering nothing is neither recorded nor reported as overlapping
// anything, wherever it lands. Such a span comes straight off the wire, since a peer is free to send a piece message
// that carries no data, and an entry covering nothing would leave the list claiming a position it doesn't hold.
func TestSpanInsertWithoutLength(t *testing.T) {
	c := check.New(t)

	// Into an empty list
	var list spanlist.SpanList
	c.False(list.Insert(&spanlist.Span{Start: 10}))
	c.Equal(0, len(list.Spans), "a span with no length must not be recorded")

	c.False(list.Insert(&spanlist.Span{Start: 10, Length: 10}))
	c.False(list.Insert(&spanlist.Span{Start: 40, Length: 10}))
	want := []spanlist.Span{{Start: 10, Length: 10}, {Start: 40, Length: 10}}

	// Ahead of, inside, at either edge of, between and beyond the existing spans
	for _, start := range []int{0, 5, 10, 15, 19, 20, 25, 40, 49, 50, 60} {
		c.False(list.Insert(&spanlist.Span{Start: start}), "no length at %d must not overlap anything", start)
		c.Equal(want, list.Spans, "no length at %d must leave the list alone", start)
	}

	// A negative length covers nothing either
	c.False(list.Insert(&spanlist.Span{Start: 15, Length: -5}))
	c.Equal(want, list.Spans)
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

// TestSpanContainsMatchesALinearSearch verifies the search that answers Contains against the straightforward walk of
// the whole list that it replaced, for every span that fits within a small universe and for a range of list shapes:
// empty, a single entry, entries with gaps on either side of them, and entries reaching each end of the universe. Only
// one entry can hold a span's start, so the answer is found by searching for that entry rather than by walking to it,
// and nothing about which entry that is may change with the shortcut.
func TestSpanContainsMatchesALinearSearch(t *testing.T) {
	c := check.New(t)
	const universe = 24
	for _, inserts := range [][]spanlist.Span{
		{},
		{{Start: 0, Length: universe}},
		{{Start: 4, Length: 4}},
		{{Start: 0, Length: 4}, {Start: 8, Length: 4}, {Start: 16, Length: 4}},
		{{Start: 2, Length: 1}, {Start: 5, Length: 6}, {Start: 20, Length: 4}},
		{
			{Start: 1, Length: 2},
			{Start: 4, Length: 1},
			{Start: 7, Length: 3},
			{Start: 12, Length: 1},
			{Start: 15, Length: 9},
		},
	} {
		var list spanlist.SpanList
		for _, one := range inserts {
			list.Insert(&one)
		}
		for start := range universe {
			for length := range universe - start + 1 {
				span := spanlist.Span{Start: start, Length: length}
				c.Equal(containsByScanning(&list, &span), list.Contains(&span), "%v in %v", span, list.Spans)
			}
		}
	}
}

// containsByScanning answers the question Contains does, the way Contains used to answer it: by walking the whole list
// looking for an entry that holds every position of the span.
func containsByScanning(sl *spanlist.SpanList, span *spanlist.Span) bool {
	if span.Length <= 0 {
		return true
	}
	for _, one := range sl.Spans {
		if span.Start >= one.Start && span.Start+span.Length <= one.Start+one.Length {
			return true
		}
	}
	return false
}

// TestSpanContains verifies which ranges are reported as already covered, which is what tells a peer whether a chunk
// that just arrived carried anything new and which chunks of a piece still have to be asked for.
func TestSpanContains(t *testing.T) {
	c := check.New(t)
	var list spanlist.SpanList
	c.False(list.Contains(&spanlist.Span{Start: 0, Length: 1}), "an empty list covers nothing")
	c.True(list.Contains(&spanlist.Span{Start: 0, Length: 0}), "a span with no length has nothing to cover")

	list.Insert(&spanlist.Span{Start: 10, Length: 10})
	list.Insert(&spanlist.Span{Start: 40, Length: 10})
	for _, one := range []struct {
		name    string
		span    spanlist.Span
		covered bool
	}{
		{name: "exactly a span", span: spanlist.Span{Start: 10, Length: 10}, covered: true},
		{name: "inside a span", span: spanlist.Span{Start: 12, Length: 5}, covered: true},
		{name: "the start of a span", span: spanlist.Span{Start: 10, Length: 1}, covered: true},
		{name: "the end of a span", span: spanlist.Span{Start: 19, Length: 1}, covered: true},
		{name: "the second span", span: spanlist.Span{Start: 40, Length: 10}, covered: true},
		{name: "overlapping the front of a span", span: spanlist.Span{Start: 9, Length: 5}, covered: false},
		{name: "overlapping the end of a span", span: spanlist.Span{Start: 15, Length: 10}, covered: false},
		{name: "spanning the gap between two spans", span: spanlist.Span{Start: 10, Length: 40}, covered: false},
		{name: "entirely within the gap", span: spanlist.Span{Start: 25, Length: 5}, covered: false},
		{name: "ahead of everything", span: spanlist.Span{Start: 0, Length: 5}, covered: false},
		{name: "beyond everything", span: spanlist.Span{Start: 50, Length: 5}, covered: false},
	} {
		c.Equal(one.covered, list.Contains(&one.span), one.name)
	}
}
