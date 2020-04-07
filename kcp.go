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
	sendWnd, recvWnd, remoteWnd, cwnd uint32
	current, interval, tsFlush, xmit  uint32
	noDelay, updated                  uint32
	probe, tsProbe, probeWait         uint32
	deadLink, incr                    uint32
	sendQueue, recvQueue              []*segment
	sendBuf, recvBuf                  []*segment
	ackList                           []uint64
	ackCount, ackBlock                uint32
	data                              []byte // only use for stream mode
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

func newSegment(size uint32) *segment {
	s := &segment{}
	s.data = make([]byte, size)
	s.data = s.data[:size]
	return s
}

func NewKCP(convID uint32, outputCallbakc OutputCallback) *KCP {
	kcp := &KCP{convID: convID, outputCallback: outputCallbakc}
	kcp.sendWnd = KCP_WND_SND
	kcp.recvWnd = KCP_WND_RCV
	kcp.remoteWnd = KCP_WND_RCV
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

// calculate RTO
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

func (kcp *KCP) parseData(newSeg *segment) {
	repeat := false
	sn := newSeg.sn
	if timediff(sn, kcp.recvNext+kcp.recvWnd) >= 0 || timediff(sn, kcp.recvNext) < 0 {
		bufferPool.Put(newSeg.data)
		newSeg.data = nil
		return
	}

	istIdx := 0
	for idx := len(kcp.recvBuf) - 1; idx >= 0; idx-- {
		seg := kcp.recvBuf[idx]
		if seg.sn == sn {
			repeat = true
			break
		}

		if timediff(sn, seg.sn) > 0 {
			istIdx = idx + 1
			break
		}
	}

	if !repeat {
		if istIdx == len(kcp.recvBuf) {
			kcp.recvBuf = append(kcp.recvBuf, newSeg)
		} else {
			kcp.recvBuf = append(kcp.recvBuf, &segment{})
			copy(kcp.recvBuf[istIdx+1:], kcp.recvBuf[istIdx:])
			kcp.recvBuf[istIdx] = newSeg
		}
	} else {
		bufferPool.Put(newSeg.data)
		newSeg.data = nil
	}

	// move available data from rcv_buf -> rcv_queue
	count := 0
	for idx := range kcp.recvBuf {
		seg := kcp.recvBuf[idx]
		if seg.sn == kcp.recvNext && len(kcp.recvBuf) < int(kcp.recvWnd) {
			count++
			kcp.recvNext++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.recvQueue = append(kcp.recvQueue, kcp.recvBuf[:count]...)
		kcp.recvBuf = removeFront(kcp.recvBuf, count)
	}
}

func (kcp *KCP) Input(data []byte) int {
	prevUNA := kcp.sendUNA
	var maxACK, latestTs uint32 = 0, 0
	flag := 0

	if data == nil || len(data) < int(KCP_OVERHEAD) {
		return -1
	}

	for {
		var ts, sn, length, una, convID uint32
		var wnd uint16
		var cmd, frg uint8
		if len(data) < int(KCP_OVERHEAD) {
			break
		}

		data = decode32u(data, &convID)
		if convID != kcp.convID {
			return -1
		}
		data = decode8u(data, &cmd)
		data = decode8u(data, &frg)
		data = decode16u(data, &wnd)
		data = decode32u(data, &ts)
		data = decode32u(data, &sn)
		data = decode32u(data, &una)
		data = decode32u(data, &length)

		if len(data) > int(length) || length < 0 {
			return -2
		}

		if uint32(cmd) != KCP_CMD_PUSH && uint32(cmd) != KCP_CMD_ACK &&
			uint32(cmd) != KCP_CMD_WASK && uint32(cmd) != KCP_CMD_WINS {
			return -3
		}

		kcp.remoteWnd = uint32(wnd)
		kcp.parseUNA(una)
		kcp.shrinkSendBuf()
		switch uint32(cmd) {
		case KCP_CMD_ACK:
			if timediff(kcp.current, ts) >= 0 {
				kcp.updateACK(uint32(timediff(kcp.current, ts)))
			}
			kcp.parseACK(sn)
			kcp.shrinkSendBuf()

			if flag == 0 {
				flag = 1
				maxACK = sn
				latestTs = ts
			} else {
				if timediff(sn, maxACK) > 0 {
					maxACK = sn
					latestTs = ts
				}
			}
		case KCP_CMD_PUSH:
			if timediff(sn, kcp.recvNext+kcp.recvWnd) < 0 {
				kcp.ackPush(sn, ts)
				if timediff(sn, kcp.recvNext) >= 0 {
					seg := newSegment(length)
					seg.convID = convID
					seg.cmd = uint32(cmd)
					seg.frg = uint32(frg)
					seg.wnd = uint32(wnd)
					seg.ts = ts
					seg.sn = sn
					seg.una = una
					if length > 0 {
						copy(seg.data, data[:length])
					}
					kcp.parseData(seg)
				}
			}
		case KCP_CMD_WASK:
			// ready to send back IKCP_CMD_WINS in ikcp_flush
			// tell remote my window size
			kcp.probe |= KCP_ASK_TELL
		case KCP_CMD_WINS:
			// do nothing
		default:
			return -3
		}

		data = data[length:]
	}

	if flag != 0 {
		kcp.parseFastACK(maxACK, latestTs)
	}

	// update local cwnd
	if timediff(kcp.sendUNA, prevUNA) > 0 {
		if kcp.cwnd < kcp.remoteWnd {
			mss := kcp.mss
			if kcp.cwnd < kcp.ssthresh {
				kcp.cwnd++
				kcp.incr += mss
			} else {
				if kcp.incr < mss {
					kcp.incr = mss
				}

				kcp.incr += (mss*mss)/kcp.incr + (mss / 16)
				if (kcp.cwnd+1)*mss <= kcp.incr {
					var tmpVar uint32 = 1
					if mss > 0 {
						tmpVar = mss
					}
					kcp.cwnd = (kcp.incr + mss - 1) / tmpVar
				}
			}

			if kcp.cwnd > kcp.remoteWnd {
				kcp.cwnd = kcp.remoteWnd
				kcp.incr = kcp.remoteWnd * mss
			}
		}
	}

	return 0
}

func (kcp *KCP) flush() {
	current := kcp.current

	if kcp.updated == 0 {
		return
	}

	seg := &segment{}
	seg.convID = kcp.convID
	seg.cmd = KCP_CMD_ACK
	seg.frg = 0
	// wnd unused
	if uint32(len(kcp.recvQueue)) < kcp.recvWnd {
		seg.wnd = kcp.recvWnd - uint32(len(kcp.recvQueue))
	}
	seg.una = kcp.recvNext
	seg.sn = 0
	seg.ts = 0

	// flush acknowledges
	for idx := range kcp.ackList {

	}
}

func (kcp *KCP) output(data []byte) {
	if len(data) <= 0 {
		return
	}

	kcp.outputCallback(data)
}
