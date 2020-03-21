package gokcp

import "sync"

type segment struct {
	convID   uint32
	cmd      uint32 // package type
	frg      uint32 // user data slice number, 0 is last
	wnd      uint32 // recv window size
	ts       uint32 // send message timestamp
	sn       uint32 // sequence number
	una      uint32 // next message sequence number
	resendTs uint32
	rto      uint32
	fastACK  uint32
	xmint    uint32
	data     []byte
	acked    bool
}

type KCP struct {
	convID, mtu, mss, state           uint32
	sendUNA, sendNext, recvNext       uint32
	recent, lastACK                   uint32
	ssthresh                          uint32
	rxRTTVal, rxSRTT, rxRTO, rxMinRTO uint32
	sendWnd, recvWnd, rmtWnd, cwnd    uint32
	current, interval, tsFlush, xmit  uint32
	noDelay, updated                  uint32
	probe, tsProbe, probeWait         uint32
	deadLink, incr                    uint32
	sendQueue, recvQueue              []*segment
	sendBuf, recvBuf                  []*segment
	ackList                           []uint64
	ackCount, ackBlock                uint32
	data                              []byte // use for stream mode
	fastResendACK, fastACKLimit       uint32
	nocwnd, streamMode                bool
	outputCallback                    OutputCallback
	mdev, mdevMax, rttSN              uint32
}

var (
	bufferPool = sync.Pool{New: func() interface{} {
		return make([]byte, KCP_MTU_DEF)
	}}
)

func NewKCP(convID uint32, outputCallbakc OutputCallback) *KCP {
	kcp := &KCP{convID: convID, outputCallback: outputCallbakc}
	kcp.sendWnd = KCP_WND_SND
	kcp.recvWnd = KCP_WND_RCV
	kcp.rmtWnd = KCP_WND_RCV
	kcp.mtu = KCP_MTU_DEF
	kcp.mss = kcp.mtu - KCP_OVERHEAD
	kcp.rxRTO = KCP_RTO_DEF
	kcp.rxMinRTO = KCP_RTO_MIN
	kcp.interval = KCP_INTERVAL
	kcp.tsFlush = KCP_INTERVAL
	kcp.ssthresh = KCP_THRESH_INIT
	kcp.fastACKLimit = KCP_FASTACK_LIMIT
	kcp.deadLink = KCP_DEADLINK
	kcp.data = make([]byte, kcp.mtu)
	return kcp
}

func (kcp *KCP) SetOutput(outputCallback OutputCallback) {
	kcp.outputCallback = outputCallback
}

func (kcp *KCP) PeekSize() (size int) {
	if len(kcp.recvQueue) <= 0 {
		return -1
	}

	seg := kcp.recvQueue[0]
	if seg.frg == 0 {
		return len(seg.data)
	}

	if len(kcp.recvQueue) < int(seg.frg+1) {
		return -1
	}

	for idx := range kcp.recvQueue {
		seg := kcp.recvQueue[idx]
		size += len(seg.data)
		if seg.frg == 0 {
			break
		}
	}

	return
}

func (kcp *KCP) Recv(buf []byte) int {
	if len(kcp.recvQueue) == 0 {
		return -1
	}

	peekSize := kcp.PeekSize()
	if peekSize < 0 {
		return -2
	}

	if peekSize > len(buf) {
		return -3
	}

	isFastRecover := false
	if len(kcp.recvQueue) >= int(kcp.recvWnd) {
		isFastRecover = true
	}

	// merge fragment
	n := 0
	count := 0
	for idx := range kcp.recvQueue {
		seg := kcp.recvQueue[idx]
		copy(buf, seg.data)
		buf = buf[:len(seg.data)]
		n += len(seg.data)
		count++
		bufferPool.Put(seg.data)

		if seg.frg == 0 {
			break
		}
	}

	if count > 0 {
		kcp.recvQueue = removeFront(kcp.recvQueue, count)
	}

	count = 0
	for idx := range kcp.recvBuf {
		seg := kcp.recvBuf[idx]
		if seg.sn == kcp.recvNext && len(kcp.recvQueue) < int(kcp.recvWnd) {
			kcp.recvQueue = append(kcp.recvQueue, seg)
			kcp.recvNext++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.recvBuf = removeFront(kcp.recvBuf, count)
	}

	// tell remote my recv window size, need to send KCP_CMD_WINS
	if (len(kcp.recvQueue) < int(kcp.recvWnd)) && isFastRecover {
		kcp.probe |= KCP_ASK_TELL
	}

	return n
}

func (kcp *KCP) Send(buf []byte) int {
	if len(buf) == 0 {
		return -1
	}

	if kcp.streamMode {
		if len(kcp.sendQueue) > 0 {
			seg := kcp.sendQueue[len(kcp.sendQueue)-1]
			if len(seg.data) < int(kcp.mss) {
				capacity := int(kcp.mss) - len(seg.data)
				extend := len(seg.data)
				if len(seg.data) > capacity {
					extend = capacity
				}

				// copy data to old segment
				orgLen := len(seg.data)
				seg.data = seg.data[:orgLen+extend]
				copy(seg.data[orgLen:], buf)
				buf = buf[extend:]
			}
		}

		if len(buf) == 0 {
			return 0
		}
	}

	count := 0
	if len(buf) <= (int)(kcp.mss) {
		count = 1
	} else {
		count = (len(buf) + int(kcp.mss) - 1) / int(kcp.mss)
	}

	if count >= int(KCP_WND_RCV) {
		return -2
	}

	if count == 0 {
		count = 1
	}

	// slice data to fregment
	for i := 0; i < count; i++ {
		size := len(buf)
		if size > int(kcp.mss) {
			size = int(kcp.mss)
		}

		seg := &segment{}
		seg.data = bufferPool.Get().([]byte)[:size]
		copy(seg.data, buf[:size])
		if kcp.streamMode {
			seg.frg = 0
		} else {
			seg.frg = uint32(count - i - 1)
		}

		kcp.sendQueue = append(kcp.sendQueue, seg)
		buf = buf[size:]
	}

	return 0
}

func (kcp *KCP) updateACK(rtt uint32) {
	if kcp.rxSRTT == 0 {
		kcp.rxSRTT = rtt
		kcp.rxRTTVal = rtt / 2 // 1/2
	} else {
		delta := rtt - kcp.rxSRTT
		if delta < 0 {
			delta = -delta
		}

		kcp.rxRTTVal = (3*kcp.rxRTTVal + delta) / 4 // 1/4
		kcp.rxSRTT = (7*kcp.rxSRTT + rtt) / 8       // 1/8
		if kcp.rxSRTT < 1 {
			kcp.rxSRTT = 1
		}
	}

	rto := kcp.rxSRTT + max(kcp.interval, 4*kcp.rxRTTVal)
	kcp.rxRTO = bound(kcp.rxMinRTO, rto, KCP_RTO_MAX)
}

func (kcp *KCP) updateACK2(rtt uint32) {
	srtt := kcp.rxSRTT
	m := rtt
	if kcp.rxSRTT == 0 {
		srtt = m << 3
		kcp.mdev = m << 1
		kcp.rxRTTVal = max(kcp.mdev, KCP_RTO_MIN)
		kcp.mdevMax = kcp.rxRTTVal
		kcp.rttSN = kcp.sendNext
	} else {
		m -= srtt >> 3
		srtt += m
		if m < 0 {
			m = -m
			m -= (kcp.mdev >> 2)
			if m > 0 {
				m >>= 3
			}
		} else {
			m -= (kcp.mdev >> 2)
		}

		kcp.mdev += m
		if kcp.mdev > kcp.mdevMax {
			kcp.mdevMax = kcp.mdev
			if kcp.mdevMax > kcp.rxRTTVal {
				kcp.rxRTTVal = kcp.mdevMax
			}

			if kcp.sendUNA > kcp.rttSN {
				if kcp.mdevMax < kcp.rxRTTVal {
					kcp.rxRTTVal -= (kcp.rxRTTVal - kcp.mdevMax) >> 2
				}

				kcp.rttSN = kcp.sendNext
				kcp.mdevMax = KCP_RTO_MIN
			}
		}
	}

	kcp.rxSRTT = max(1, srtt)
	rto := kcp.rxSRTT*1 + 4*kcp.mdev
	kcp.rxRTO = bound(kcp.rxMinRTO, rto, KCP_RTO_MIN)
}

func (kcp *KCP) shrinkSendBuf() {
	if len(kcp.sendBuf) > 0 {
		seg := kcp.sendBuf[0]
		kcp.sendUNA = seg.sn
	} else {
		kcp.sendUNA = kcp.sendNext
	}
}

func (kcp *KCP) parseACK(sn uint32) {
	if timediff(sn, kcp.sendUNA) < 0 || timediff(sn, kcp.sendNext) >= 0 {
		return
	}

	for idx := range kcp.sendBuf {
		seg := kcp.sendBuf[idx]
		if sn == seg.sn {
			seg.acked = true
			bufferPool.Put(seg.data)
			seg.data = nil
			break
		}

		if timediff(sn, seg.sn) < 0 {
			break
		}
	}
}

func (kcp *KCP) parseUNA(una uint32) {
	count := 0
	for idx := range kcp.sendBuf {
		seg := kcp.sendBuf[idx]
		if timediff(una, seg.sn) > 0 {
			bufferPool.Put(seg.data)
			seg.data = nil
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.sendBuf = removeFront(kcp.sendBuf, count)
	}
}

func (kcp *KCP) parseFastACK(sn uint32, ts uint32) {
	if timediff(sn, kcp.sendUNA) < 0 || timediff(sn, kcp.sendNext) >= 0 {
		return
	}

	for idx := range kcp.sendBuf {
		seg := kcp.sendBuf[idx]
		if timediff(sn, seg.sn) < 0 {
			break
		} else if sn != seg.sn {
			if timediff(ts, seg.ts) >= 0 {
				seg.fastACK++
			}
		}
	}
}

func (kcp *KCP) ackPush(sn, ts uint32) {
	kcp.ackList = append(kcp.ackList, packACK(sn, ts))
}

func (kcp *KCP) getACK(idx int) (sn, ts uint32) {
	sn, ts = unpackACK(kcp.ackList[idx])
	return
}

func (kcp *KCP) parseData(seg *segment) {

}
