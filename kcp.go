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
	xmit     uint32
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
	noDelay                           bool
	updated                           uint32
	probe, tsProbe, probeWait         uint32
	deadLink, incr                    uint32
	sendQueue, recvQueue              []*segment
	sendBuffer, recvBuffer            []*segment
	ackList                           []uint64
	ackCount, ackBlock                uint32
	dataBuffer                        []byte
	buffer                            *Buffer
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
	kcp.dataBuffer = make([]byte, kcp.mtu)
	kcp.dataBuffer = kcp.dataBuffer[:0]
	kcp.buffer = NewBuffer(int(kcp.mtu))
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
	for idx := range kcp.recvBuffer {
		seg := kcp.recvBuffer[idx]
		if seg.sn == kcp.recvNext && len(kcp.recvQueue) < int(kcp.recvWnd) {
			kcp.recvQueue = append(kcp.recvQueue, seg)
			kcp.recvNext++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.recvBuffer = removeFront(kcp.recvBuffer, count)
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
	if len(kcp.sendBuffer) > 0 {
		seg := kcp.sendBuffer[0]
		kcp.sendUNA = seg.sn
	} else {
		kcp.sendUNA = kcp.sendNext
	}
}

func (kcp *KCP) parseACK(sn uint32) {
	if timediff(sn, kcp.sendUNA) < 0 || timediff(sn, kcp.sendNext) >= 0 {
		return
	}

	for idx := range kcp.sendBuffer {
		seg := kcp.sendBuffer[idx]
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
	for idx := range kcp.sendBuffer {
		seg := kcp.sendBuffer[idx]
		if timediff(una, seg.sn) > 0 {
			bufferPool.Put(seg.data)
			seg.data = nil
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.sendBuffer = removeFront(kcp.sendBuffer, count)
	}
}

func (kcp *KCP) parseFastACK(sn uint32, ts uint32) {
	if timediff(sn, kcp.sendUNA) < 0 || timediff(sn, kcp.sendNext) >= 0 {
		return
	}

	for idx := range kcp.sendBuffer {
		seg := kcp.sendBuffer[idx]
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
	for idx := len(kcp.recvBuffer) - 1; idx >= 0; idx-- {
		seg := kcp.recvBuffer[idx]
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
		if istIdx == len(kcp.recvBuffer) {
			kcp.recvBuffer = append(kcp.recvBuffer, newSeg)
		} else {
			kcp.recvBuffer = append(kcp.recvBuffer, &segment{})
			copy(kcp.recvBuffer[istIdx+1:], kcp.recvBuffer[istIdx:])
			kcp.recvBuffer[istIdx] = newSeg
		}
	} else {
		bufferPool.Put(newSeg.data)
		newSeg.data = nil
	}

	// move available data from rcv_buf -> rcv_queue
	count := 0
	for idx := range kcp.recvBuffer {
		seg := kcp.recvBuffer[idx]
		if seg.sn == kcp.recvNext && len(kcp.recvBuffer) < int(kcp.recvWnd) {
			count++
			kcp.recvNext++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.recvQueue = append(kcp.recvQueue, kcp.recvBuffer[:count]...)
		kcp.recvBuffer = removeFront(kcp.recvBuffer, count)
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
		if kcp.buffer.Len()+int(KCP_OVERHEAD) > int(kcp.mtu) {
			kcp.output(kcp.buffer.Data())
			kcp.buffer.Reset()
		}

		seg.sn, seg.ts = unpackACK(kcp.ackList[idx])
		kcp.buffer.WriteOverHeader(seg)
	}
	kcp.ackList = kcp.ackList[:0]

	// probe window size (if remote window size equals zero)
	if kcp.remoteWnd == 0 {
		if kcp.probeWait == 0 {
			kcp.probeWait = KCP_PROBE_INIT
			kcp.tsProbe = kcp.current + kcp.probeWait
		} else {
			if timediff(kcp.current, kcp.probeWait) >= 0 {
				if kcp.probeWait < KCP_PROBE_INIT {
					kcp.probeWait = KCP_PROBE_INIT
				}
				kcp.probeWait += kcp.probeWait / 2

				if kcp.probeWait > KCP_PROBE_LIMIT {
					kcp.probeWait = KCP_PROBE_LIMIT
				}
				kcp.tsProbe = kcp.current + kcp.probeWait
				kcp.probe |= KCP_ASK_SEND
			}
		}
	} else {
		kcp.tsProbe = 0
		kcp.probeWait = 0
	}

	// flush window probing commands
	if (kcp.probe & KCP_ASK_SEND) != 0 {
		seg.cmd = KCP_CMD_WASK
		if kcp.buffer.Len()+int(KCP_OVERHEAD) > int(kcp.mtu) {
			kcp.output(kcp.buffer.Data())
			kcp.buffer.Reset()
		}

		kcp.buffer.WriteOverHeader(seg)
	}

	if (kcp.probe & KCP_ASK_TELL) != 0 {
		seg.cmd = KCP_CMD_WINS
		if kcp.buffer.Len()+int(KCP_OVERHEAD) > int(kcp.mtu) {
			kcp.output(kcp.buffer.Data())
			kcp.buffer.Reset()
		}

		kcp.buffer.WriteOverHeader(seg)
	}
	kcp.probe = 0

	// calculate window size
	cwnd := min(kcp.sendWnd, kcp.remoteWnd)
	if !kcp.nocwnd {
		cwnd = min(kcp.cwnd, cwnd)
	}

	// move data from sendQueue to sendBuffer
	count := 0
	for idx := 0; idx < len(kcp.sendQueue); idx++ {
		if timediff(kcp.sendNext, kcp.sendUNA+cwnd) >= 0 {
			break
		}

		newseg := kcp.sendQueue[idx]
		newseg.convID = kcp.convID
		newseg.cmd = KCP_CMD_PUSH
		newseg.wnd = seg.wnd
		newseg.ts = current
		newseg.sn = kcp.sendNext
		newseg.una = kcp.recvNext
		newseg.resendTs = current
		newseg.rto = kcp.rxRTO
		newseg.fastACK = 0
		newseg.xmit = 0
		kcp.sendBuffer = append(kcp.sendBuffer, newseg)
		kcp.sendNext++
		count++
	}
	if count > 0 {
		kcp.sendQueue = removeFront(kcp.sendQueue, count)
	}

	// calculate resent
	var resent uint32
	if kcp.fastResendACK > 0 {
		resent = kcp.fastResendACK
	} else {
		resent = 0xffffffff
	}

	var minRTO uint32
	if !kcp.noDelay {
		minRTO = kcp.rxRTO >> 3
	} else {
		minRTO = 0
	}

	// flush data segments
	lost := false
	change := 0
	for idx := 0; idx < len(kcp.sendBuffer); idx++ {
		sendSegment := kcp.sendBuffer[idx]
		needSend := false
		// first send
		if sendSegment.xmit == 0 {
			needSend = true
			sendSegment.xmit++
			sendSegment.rto = kcp.rxRTO
			sendSegment.resendTs = current + sendSegment.rto + minRTO
		} else if timediff(current, sendSegment.resendTs) >= 0 {
			needSend = true
			sendSegment.xmit++
			kcp.xmit++
			if !kcp.noDelay {
				sendSegment.rto += kcp.rxRTO
			} else {
				sendSegment.rto += kcp.rxRTO / 2
			}
			sendSegment.resendTs = current + sendSegment.rto
			lost = true
		} else if sendSegment.fastACK >= resent {
			if sendSegment.xmit <= kcp.fastACKLimit || kcp.fastACKLimit <= 0 {
				needSend = true
				sendSegment.xmit++
				sendSegment.fastACK = 0
				sendSegment.resendTs = current + sendSegment.rto
				change++
			}
		}

		if needSend {
			sendSegment.ts = current
			sendSegment.wnd = seg.wnd
			sendSegment.una = kcp.recvNext

			if kcp.buffer.Len()+int(KCP_OVERHEAD)+len(sendSegment.data) > int(kcp.mtu) {
				kcp.output(kcp.buffer.Data())
				kcp.buffer.Reset()
			}

			kcp.buffer.WriteOverHeader(seg)
			if int(KCP_OVERHEAD)+len(sendSegment.data) > int(kcp.mtu) {
				kcp.output(kcp.buffer.Data())
				kcp.buffer.Reset()
			}

			if len(sendSegment.data) > 0 {
				kcp.buffer.Write(sendSegment.data)
			}

			if sendSegment.xmit >= kcp.deadLink {
				kcp.state = -1
			}
		}
	}

	// flash remain segments
	if kcp.buffer.Len() > 0 {
		kcp.output(kcp.buffer.Data())
		kcp.buffer.Reset()
	}

	// update ssthresh
	// rate halving, https://tools.ietf.org/html/rfc6937
	if change > 0 {
		inflight := kcp.sendNext - kcp.sendUNA
		kcp.ssthresh = inflight / 2
		if kcp.ssthresh < KCP_THRESH_MIN {
			kcp.ssthresh = KCP_THRESH_MIN
		}
		kcp.cwnd = kcp.ssthresh + resent
		kcp.incr = kcp.cwnd * kcp.mss
	}

	// if some package lost
	if lost {
		kcp.ssthresh = cwnd / 2
		if kcp.ssthresh < KCP_THRESH_MIN {
			kcp.ssthresh = KCP_THRESH_MIN
		}
		kcp.cwnd = 1
		kcp.incr = kcp.mss
	}

	if kcp.cwnd < 1 {
		kcp.cwnd = 1
		kcp.incr = kcp.mss
	}
}

func (kcp *KCP) output(data []byte) {
	if len(data) <= 0 {
		return
	}

	kcp.outputCallback(data)
}
