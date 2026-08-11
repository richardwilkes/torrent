// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package spanlist

import (
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
)

// TestSpanOverlaps verifies which pairs of spans are considered to have a position in common, which is what tells
// Insert whether the span it was handed brought anything that was already there. Spans that merely abut each other
// don't overlap, and a span with no length holds no position, so it can't overlap anything no matter where it sits.
func TestSpanOverlaps(t *testing.T) {
	c := check.New(t)
	base := Span{Start: 10, Length: 10}
	for _, one := range []struct {
		name  string
		other Span
		want  bool
	}{
		{name: "the same span", other: Span{Start: 10, Length: 10}, want: true},
		{name: "wholly inside it", other: Span{Start: 12, Length: 5}, want: true},
		{name: "wholly around it", other: Span{Start: 0, Length: 30}, want: true},
		{name: "over its start", other: Span{Start: 5, Length: 6}, want: true},
		{name: "over its end", other: Span{Start: 19, Length: 6}, want: true},
		{name: "abutting its start", other: Span{Start: 5, Length: 5}},
		{name: "abutting its end", other: Span{Start: 20, Length: 5}},
		{name: "ahead of it", other: Span{Start: 0, Length: 5}},
		{name: "beyond it", other: Span{Start: 25, Length: 5}},
		{name: "no length, inside it", other: Span{Start: 15}},
		{name: "no length, at its start", other: Span{Start: 10}},
		{name: "no length, at its end", other: Span{Start: 20}},
		{name: "negative length, inside it", other: Span{Start: 15, Length: -5}},
	} {
		c.Equal(one.want, base.overlaps(&one.other), one.name)
		c.Equal(one.want, one.other.overlaps(&base), one.name+", reversed")
	}
	empty := Span{Start: 10}
	c.False(empty.overlaps(&empty), "a span with no length doesn't even overlap itself")
}
