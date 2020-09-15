package gokcp

import (
	"time"
	_ "unsafe"
)

const (
	KCP_RTO_NDL       uint32 = 30    // no delay model min rto
	KCP_RTO_MIN       uint32 = 100   // normal model min rto
	KCP_RTO_DEF       uint32 = 200   // rto
	KCP_RTO_MAX       uint32 = 60000 // max rto
	KCP_CMD_PUSH      uint32 = 81    // cmd: push data to remote
	KCP_CMD_ACK       uint32 = 82    // cmd: ack
	KCP_CMD_WASK      uint32 = 83    // cmd: window probe(ask)
	KCP_CMD_WINS      uint32 = 94    // cmd: window size(tell)
	KCP_ASK_SEND      uint32 = 1     // need to send KCP_CMD_WASK
	KCP_ASK_TELL      uint32 = 2     // need to send KCP_CMD_WINS
	KCP_WND_SND       uint32 = 32    // send window size, uint package not bytes length
	KCP_WND_RCV       uint32 = 128   // recv windows size, uint package not bytes length
	KCP_MTU_DEF       uint32 = 1400  // mtu
	KCP_ACK_FAST      uint32 = 3
	KCP_INTERVAL      uint32 = 100 // update interval
	KCP_OVERHEAD      uint32 = 24  // kcp package head size
	KCP_DEADLINK      uint32 = 20
	KCP_THRESH_INIT   uint32 = 2
	KCP_THRESH_MIN    uint32 = 2
	KCP_PROBE_INIT    uint32 = 7000   // 7 secs to probe window size
	KCP_PROBE_LIMIT   uint32 = 120000 // up to 120 secs to probe window
	KCP_FASTACK_LIMIT uint32 = 5      // max times to trigger fastack
)

type OutputCallback func(p []byte) error

const PackBits = 32

func unpackACK(ack uint64) (sn, ts uint32) {
	const mask = 1<<PackBits - 1
	sn = uint32((ack >> PackBits) & mask)
	ts = uint32(ack & mask)
	return
}

func packACK(sn, ts uint32) uint64 {
	const mask = 1<<PackBits - 1
	return (uint64(sn) << PackBits) | uint64(ts&mask)
}

func removeFront(p []*segment, count int) []*segment {
	if count > len(p) {
		p = p[:0]
	} else {
		p = p[count:]
	}

	return p
}

//go:linkname nanotime runtime.nanotime
func nanotime() int64

var kcpStartTime = nanotime()

func SetupFromNowMS() uint32 {
	return uint32((nanotime() - kcpStartTime) / (1000 * 1000))
}

func NowMS() int64 {
	now := time.Now()
	return now.Unix()*1000 + int64(now.Nanosecond()/1e6)
}
