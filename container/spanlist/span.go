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
// not overlap.
func (s *Span) overlaps(other *Span) bool {
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

// Insert a span into the list. Returns true if the span overlapped an
// existing span within the list. Note that the new span may reach past the
// first span it touches, so every span it is merged with has to be considered,
// not just the first one.
func (sl *SpanList) Insert(span *Span) bool {
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
