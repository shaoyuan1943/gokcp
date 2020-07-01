package gokcp

import "sync"

var (
	segmentPool = sync.Pool{New: func() interface{} {
		s := &segment{}
		s.dataBuffer = make([]byte, KCP_MTU_DEF)
		s.dataBuffer = s.dataBuffer[:0]
		return s
	}}

	bufferPool = sync.Pool{New: func() interface{} {
		buffer := make([]byte, KCP_MTU_DEF)
		return buffer
	}}
)

func getSegment() *segment {
	return segmentPool.Get().(*segment)
}

func putSegment(seg *segment) {
	seg.reset()
	segmentPool.Put(seg)
}
