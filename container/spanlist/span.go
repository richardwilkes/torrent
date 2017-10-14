package spanlist

// Span holds a starting position and a length.
type Span struct {
	Start  int
	Length int
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
// existing span within the list.
func (sl *SpanList) Insert(span *Span) bool {
	for i, one := range sl.Spans {
		// Before
		if span.Start+span.Length < one.Start {
			spans := make([]Span, len(sl.Spans)+1)
			copy(spans[1:], sl.Spans)
			spans[0] = *span
			sl.Spans = spans
			return false
		}
		// Overlap or abut
		if span.Start <= one.Start+one.Length {
			hadOverlap := span.Start < one.Start+one.Length && span.Start+span.Length > one.Start
			sl.Spans[i].merge(span)
			for i < len(sl.Spans)-1 && sl.Spans[i].Start+sl.Spans[i].Length >= sl.Spans[i+1].Start {
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
