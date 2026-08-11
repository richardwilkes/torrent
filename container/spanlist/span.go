// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package spanlist

import "slices"

// Span holds a starting position and a length.
type Span struct {
	Start  int
	Length int
}

// overlaps returns true if the two spans have at least one position in common. Spans that merely abut each other do
// not overlap, and a span with no length overlaps nothing at all, wherever it sits, since it holds no position that
// something else could also hold.
func (s *Span) overlaps(other *Span) bool {
	if s.Length <= 0 || other.Length <= 0 {
		return false
	}
	return s.Start < other.Start+other.Length && other.Start < s.Start+s.Length
}

func (s *Span) merge(other *Span) {
	e := s.Start + s.Length
	oe := other.Start + other.Length
	if e < oe {
		e = oe
	}
	if s.Start > other.Start {
		s.Start = other.Start
	}
	s.Length = e - s.Start
}

// SpanList holds a list of spans.
type SpanList struct {
	Spans []Span
}

// Contains returns true if every position in the span is already covered by the list. A span with no length is
// contained by definition, since there is nothing in it that could be missing. Note that spans that overlap or abut
// are merged as they are inserted, so a span that is covered at all is covered by a single entry.
func (sl *SpanList) Contains(span *Span) bool {
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

// Insert a span into the list. Returns true if the span overlapped an
// existing span within the list. Note that the new span may reach past the
// first span it touches, so every span it is merged with has to be considered,
// not just the first one. A span with no length covers no position, so there is
// nothing of it to record and nothing it could have overlapped. Such a span
// arrives straight from the network, since a peer is free to send a piece
// message that carries no data.
func (sl *SpanList) Insert(span *Span) bool {
	if span.Length <= 0 {
		return false
	}
	for i, one := range sl.Spans {
		// Before
		if span.Start+span.Length < one.Start {
			sl.Spans = slices.Insert(sl.Spans, i, *span)
			return false
		}
		// Overlap or abut
		if span.Start <= one.Start+one.Length {
			hadOverlap := span.overlaps(&one)
			sl.Spans[i].merge(span)
			for i < len(sl.Spans)-1 && sl.Spans[i].Start+sl.Spans[i].Length >= sl.Spans[i+1].Start {
				hadOverlap = hadOverlap || span.overlaps(&sl.Spans[i+1])
				sl.Spans[i].merge(&sl.Spans[i+1])
				if i == len(sl.Spans)-2 {
					sl.Spans = sl.Spans[:len(sl.Spans)-1]
					break
				}
				sl.Spans = append(sl.Spans[:i+1], sl.Spans[i+2:]...)
			}
			return hadOverlap
		}
	}
	sl.Spans = append(sl.Spans, *span)
	return false
}
