package torrent

type span struct {
	start  int
	length int
}

func (r span) insertInto(list []span) ([]span, bool) {
	for i, one := range list {
		// Before
		if r.start+r.length < one.start {
			newList := make([]span, len(list)+1)
			copy(newList[1:], list)
			newList[0] = r
			return newList, false
		}
		// Overlap or abut
		if r.start <= one.start+one.length {
			hadOverlap := r.start < one.start+one.length && r.start+r.length > one.start
			list[i].merge(r)
			for i < len(list)-1 && list[i].start+list[i].length >= list[i+1].start {
				list[i].merge(list[i+1])
				if i == len(list)-2 {
					return list[:len(list)-1], hadOverlap
				}
				list = append(list[:i+1], list[i+2:]...)
			}
			return list, hadOverlap
		}
	}
	return append(list, r), false
}

func (r *span) merge(other span) {
	e := r.start + r.length
	oe := other.start + other.length
	if e < oe {
		e = oe
	}
	if r.start > other.start {
		r.start = other.start
	}
	r.length = e - r.start
}
