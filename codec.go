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
