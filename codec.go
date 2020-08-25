package gokcp

import "encoding/binary"

func encode8u(p []byte, c byte) []byte {
	p[0] = c
	return p[1:]
}

func decode8u(p []byte, c *byte) []byte {
	*c = p[0]
	return p[1:]
}

func encode16u(p []byte, w uint16) []byte {
	binary.LittleEndian.PutUint16(p, w)
	return p[2:]
}

func decode16u(p []byte, w *uint16) []byte {
	*w = binary.LittleEndian.Uint16(p)
	return p[2:]
}

func encode32u(p []byte, l uint32) []byte {
	binary.LittleEndian.PutUint32(p, l)
	return p[4:]
}

func decode32u(p []byte, l *uint32) []byte {
	*l = binary.LittleEndian.Uint32(p)
	return p[4:]
}

func encode64u(p []byte, l uint64) []byte {
	binary.LittleEndian.PutUint64(p, l)
	return p[8:]
}

func decode64u(p []byte, l *uint64) []byte {
	*l = binary.LittleEndian.Uint64(p)
	return p[8:]
}

func min(a, b uint32) uint32 {
	if a <= b {
		return a
	}
	return b
}

func max(a, b uint32) uint32 {
	if a >= b {
		return a
	}
	return b
}

func bound(lower, middle, upper uint32) uint32 {
	return min(max(lower, middle), upper)
}

func timediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

func encodeSegment(data []byte, seg *segment) []byte {
	rawseg := encode32u(data, seg.convID)                   // 4
	rawseg = encode8u(rawseg, uint8(seg.cmd))               // 1
	rawseg = encode8u(rawseg, uint8(seg.frg))               // 1
	rawseg = encode16u(rawseg, uint16(seg.wnd))             // 2
	rawseg = encode32u(rawseg, seg.ts)                      // 4
	rawseg = encode32u(rawseg, seg.sn)                      // 4
	rawseg = encode32u(rawseg, seg.una)                     // 4
	rawseg = encode32u(rawseg, uint32(len(seg.dataBuffer))) // 4
	return rawseg                                           // total 24
}
