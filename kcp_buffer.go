package gokcp

import (
	"encoding/binary"
)

type Buffer struct {
	buffer    []byte
	segBuffer []byte
}

func NewBuffer(size int) *Buffer {
	b := &Buffer{
		buffer:    make([]byte, size),
		segBuffer: make([]byte, KCP_OVERHEAD),
	}

	b.buffer = b.buffer[:0]
	return b
}

func (b *Buffer) Reset() {
	b.buffer = b.buffer[:0]
}

func (b *Buffer) Write(p []byte) {
	b.buffer = append(b.buffer, p...)
}

func (b *Buffer) WriteOverHeader(seg *segment) {
	binary.LittleEndian.PutUint32(b.segBuffer, seg.convID)
	b.segBuffer[4] = byte(seg.cmd)
	b.segBuffer[5] = byte(seg.frg)
	binary.LittleEndian.PutUint16(b.segBuffer[6:], uint16(seg.wnd))
	binary.LittleEndian.PutUint32(b.segBuffer[8:], seg.ts)
	binary.LittleEndian.PutUint32(b.segBuffer[12:], seg.sn)
	binary.LittleEndian.PutUint32(b.segBuffer[16:], seg.una)
	binary.LittleEndian.PutUint32(b.segBuffer[20:], uint32(len(seg.dataBuffer)))
	b.segBuffer = b.segBuffer[0:]
	b.Write(b.segBuffer)
}

func (b *Buffer) CompareDiff(size int) bool {
	return len(b.buffer) > size
}

func (b *Buffer) Data() []byte {
	return b.buffer
}

func (b *Buffer) Len() int {
	return len(b.buffer)
}
