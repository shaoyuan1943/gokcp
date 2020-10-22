package gokcp

import "sync"

var (
	segmentPool sync.Pool
)

// size is MSS
func getSegment(size uint32) *segment {
	if segmentPool.New == nil {
		segmentPool.New = func() interface{} {
			s := &segment{}
			s.dataBuffer = make([]byte, int(size))
			s.dataBuffer = s.dataBuffer[:0]
			return s
		}
	}

	return segmentPool.Get().(*segment)
}

func putSegment(seg *segment) {
	seg.reset()
	segmentPool.Put(seg)
}
