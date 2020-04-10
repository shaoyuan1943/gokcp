package gokcp

import "sync"

var (
	segmentPool = sync.Pool{New: func() interface{} {
		s := &segment{}
		s.data = make([]byte, KCP_MTU_DEF)
		s.data = s.data[:0]
		return s
	}}
)

func getSegment() *segment {
	return segmentPool.Get().(*segment)
}

func putSegment(seg *segment) {
	seg.reset()
	segmentPool.Put(seg)
}
