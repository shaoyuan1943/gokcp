package gokcp

import (
	"time"
)

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

func (seg *segment) reset() {
	seg.convID = 0
	seg.cmd = 0
	seg.frg = 0
	seg.wnd = 0
	seg.ts = 0
	seg.sn = 0
	seg.una = 0
	seg.resendTs = 0
	seg.rto = 0
	seg.fastACK = 0
	seg.xmit = 0
	seg.data = seg.data[:0]
	seg.acked = false
}

type KCP struct {
	convID, mtu, mss, state           uint32
	sendUNA, sendNext, recvNext       uint32
	recent, lastACK                   uint32
	ssthresh                          uint32
	rxRTTVal, rxSRTT, rxRTO, rxMinRTO uint32
	sendWnd, recvWnd, remoteWnd, cwnd uint32
	interval, tsFlush, xmit           uint32
	noDelay                           bool
	updated                           uint32
	probe, tsProbe, probeWait         uint32
	deadLink, incr                    uint32
	sendQueue, recvQueue              []*segment
	sendBuffer, recvBuffer            []*segment
	ackList                           []uint64
	buffer                            *Buffer
	fastResendACK, fastACKLimit       uint32
	nocwnd, streamMode                bool
	outputCallback                    OutputCallback
	mdev, mdevMax, rttSN              uint32
}

var startTime time.Time = time.Now()

func millisecond() uint32 {
	return uint32(time.Now().Sub(startTime) / time.Millisecond)
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
	kcp.buffer = NewBuffer(int(kcp.mtu+KCP_OVERHEAD) * 3)
	return kcp
}

func (kcp *KCP) SetOutput(outputCallback OutputCallback) {
	kcp.outputCallback = outputCallback
}

// calculate a message data size
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

// read a whole message packet
// User or upper level recv: returns size, returns below zero for EAGAIN
func (kcp *KCP) Recv(buffer []byte) int {
	if len(kcp.recvQueue) == 0 {
		return -1
	}

	peekSize := kcp.PeekSize()
	if peekSize < 0 {
		return -1
	}

	if peekSize > len(buffer) {
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
		frg := seg.frg
		copy(buffer, seg.data)
		buffer = buffer[:len(seg.data)]
		n += len(seg.data)
		count++

		putSegment(seg)
		if frg == 0 {
			break
		}
	}

	if count > 0 {
		kcp.recvQueue = removeFront(kcp.recvQueue, count)
	}

	// move seg from recvBuffer to recvQueue
	count = 0
	for idx := range kcp.recvBuffer {
		seg := kcp.recvBuffer[idx]
		if seg.sn == kcp.recvNext && len(kcp.recvQueue)+count < int(kcp.recvWnd) {
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
		// ready to send back IKCP_CMD_WINS in ikcp_flush
		// tell remote my window size in next 'flush'
		kcp.probe |= KCP_ASK_TELL
	}

	return n
}

func (kcp *KCP) Send(buffer []byte) int {
	if len(buffer) == 0 {
		return -1
	}
	// append to previous segment in streaming mode (if possible)
	if kcp.streamMode {
		if len(kcp.sendQueue) > 0 {
			seg := kcp.sendQueue[len(kcp.sendQueue)-1]
			if len(seg.data) < int(kcp.mss) {
				capacity := int(kcp.mss) - len(seg.data)
				extend := 0
				if len(buffer) < capacity {
					extend = len(buffer)
				} else {
					extend = capacity
				}

				// copy data to old segment
				orgLen := len(seg.data)
				seg.data = seg.data[:orgLen+extend]
				copy(seg.data[orgLen:], buffer)
				buffer = buffer[extend:]
			}
		}

		if len(buffer) == 0 {
			return 0
		}
	}

	count := 0
	if len(buffer) <= (int)(kcp.mss) {
		count = 1
	} else {
		count = (len(buffer) + int(kcp.mss) - 1) / int(kcp.mss)
	}

	if count >= int(KCP_WND_RCV) {
		return -2
	}

	if count == 0 {
		count = 1
	}

	// slice data to fregment
	for i := 0; i < count; i++ {
		size := len(buffer)
		if size > int(kcp.mss) {
			size = int(kcp.mss)
		}

		seg := getSegment()
		seg.data = seg.data[:size]
		copy(seg.data, buffer[:size])
		if kcp.streamMode {
			seg.frg = 0
		} else {
			seg.frg = uint32(count - i - 1)
		}

		kcp.sendQueue = append(kcp.sendQueue, seg)
		buffer = buffer[size:]
	}

	return 0
}

// calculate RTO
// RFC6298：http://tools.ietf.org/html/rfc6298
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

// calculate RTO
// updateACK2 port from tcp_input.c 'tcp_rtt_estimator' function
// https://github.com/torvalds/linux/blob/master/net/ipv4/tcp_input.c
func (kcp *KCP) updateACK2(rtt uint32) {
	srtt := kcp.rxSRTT
	m := rtt
	if kcp.rxSRTT == 0 {
		srtt = m << 3
		kcp.mdev = m << 1 // make sure rto = 3*rtt
		kcp.rxRTTVal = max(kcp.mdev, KCP_RTO_MIN)
		kcp.mdevMax = kcp.rxRTTVal
		kcp.rttSN = kcp.sendNext
	} else {
		m -= srtt >> 3
		srtt += m // rtt = 7/8 rtt + 1/8 new
		if m < 0 {
			m = -m // abs
			m -= (kcp.mdev >> 2)
			if m > 0 {
				m >>= 3
			}
		} else {
			m -= (kcp.mdev >> 2)
		}

		kcp.mdev += m // mdev = 3/4 mdev + 1/4 new
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

func (kcp *KCP) setSendUNA() {
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
			putSegment(seg)
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

func (kcp *KCP) parseData(newseg *segment) {
	repeat := false
	sn := newseg.sn
	if timediff(sn, kcp.recvNext+kcp.recvWnd) >= 0 || timediff(sn, kcp.recvNext) < 0 {
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
			kcp.recvBuffer = append(kcp.recvBuffer, newseg)
		} else {
			kcp.recvBuffer = append(kcp.recvBuffer, &segment{})
			copy(kcp.recvBuffer[istIdx+1:], kcp.recvBuffer[istIdx:])
			kcp.recvBuffer[istIdx] = newseg
		}
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

	currentTime := millisecond()
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
		kcp.setSendUNA()
		switch uint32(cmd) {
		case KCP_CMD_ACK:
			if timediff(currentTime, ts) >= 0 {
				kcp.updateACK(uint32(timediff(currentTime, ts)))
			}
			kcp.parseACK(sn)
			kcp.setSendUNA()

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
					seg := getSegment()
					seg.data = seg.data[:length]
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
	currentTime := millisecond()

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
			kcp.tsProbe = currentTime + kcp.probeWait
		} else {
			if timediff(currentTime, kcp.probeWait) >= 0 {
				if kcp.probeWait < KCP_PROBE_INIT {
					kcp.probeWait = KCP_PROBE_INIT
				}
				kcp.probeWait += kcp.probeWait / 2

				if kcp.probeWait > KCP_PROBE_LIMIT {
					kcp.probeWait = KCP_PROBE_LIMIT
				}
				kcp.tsProbe = currentTime + kcp.probeWait
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
		newseg.ts = currentTime
		newseg.sn = kcp.sendNext
		newseg.una = kcp.recvNext
		newseg.resendTs = currentTime
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
		if sendSegment.acked {
			continue
		}

		// first send
		if sendSegment.xmit == 0 {
			needSend = true
			sendSegment.xmit++
			sendSegment.rto = kcp.rxRTO
			sendSegment.resendTs = currentTime + sendSegment.rto + minRTO
		} else if timediff(currentTime, sendSegment.resendTs) >= 0 {
			needSend = true
			sendSegment.xmit++
			kcp.xmit++
			if !kcp.noDelay {
				sendSegment.rto += kcp.rxRTO
			} else {
				sendSegment.rto += kcp.rxRTO / 2
			}
			sendSegment.resendTs = currentTime + sendSegment.rto
			lost = true
		} else if sendSegment.fastACK >= resent {
			if sendSegment.xmit <= kcp.fastACKLimit || kcp.fastACKLimit <= 0 {
				needSend = true
				sendSegment.xmit++
				sendSegment.fastACK = 0
				sendSegment.resendTs = currentTime + sendSegment.rto
				change++
			}
		}

		if needSend {
			sendSegment.ts = currentTime
			sendSegment.wnd = seg.wnd
			sendSegment.una = kcp.recvNext

			if kcp.buffer.Len()+int(KCP_OVERHEAD)+len(sendSegment.data) > int(kcp.mtu) {
				kcp.output(kcp.buffer.Data())
				kcp.buffer.Reset()
			}

			kcp.buffer.WriteOverHeader(seg)
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

//---------------------------------------------------------------------
// update state (call it repeatedly, every 10ms-100ms), or you can ask
// 'Check' when to call it again (without Input/Send calling).
//---------------------------------------------------------------------
func (kcp *KCP) Update() {
	currentTime := millisecond()
	if kcp.updated == 0 {
		kcp.updated = 1
		kcp.tsFlush = currentTime
	}

	slap := timediff(currentTime, kcp.tsFlush)
	if slap >= 10000 || slap < -10000 {
		kcp.tsFlush = currentTime
		slap = 0
	}

	if slap >= 0 {
		kcp.tsFlush += kcp.interval
		if timediff(currentTime, kcp.tsFlush) >= 0 {
			kcp.tsFlush = currentTime + kcp.interval
		}
		kcp.flush()
	}
}

func (kcp *KCP) SetMTU(mtu int) {
	if mtu < 50 || mtu < int(KCP_OVERHEAD) {
		return
	}

	kcp.mtu = uint32(mtu)
	kcp.mss = kcp.mtu - KCP_OVERHEAD
	kcp.buffer = nil
	kcp.buffer = NewBuffer((mtu + int(KCP_OVERHEAD)) * 3)
}

func (kcp *KCP) SetInterval(interval int) {
	if interval > 5000 {
		interval = 5000
	} else if interval < 10 {
		interval = 10
	}

	kcp.interval = uint32(interval)
}

//---------------------------------------------------------------------
// Determine when should you invoke ikcp_update:
// returns when you should invoke ikcp_update in millisec, if there
// is no ikcp_input/_send calling. you can call ikcp_update in that
// time, instead of call update repeatly.

// Important to reduce unnacessary ikcp_update invoking. use it to
// schedule ikcp_update (eg. implementing an epoll-like mechanism,
// or optimize ikcp_update when handling massive kcp connections)
// wiki: https://github.com/skywind3000/kcp/wiki/KCP-Best-Practice
//---------------------------------------------------------------------
func (kcp *KCP) Check() uint32 {
	currentTime := millisecond()
	if kcp.updated == 0 {
		return currentTime
	}

	tsFlush := kcp.tsFlush
	if timediff(currentTime, tsFlush) >= 10000 || timediff(currentTime, tsFlush) < -10000 {
		tsFlush = currentTime
	}

	if timediff(currentTime, tsFlush) >= 0 {
		return currentTime
	}

	tmPacket := 0x7FFFFFFF
	tmFlush := 0x7FFFFFFF
	minimal := 0
	tsFlush = uint32(timediff(tsFlush, currentTime))
	for idx := range kcp.sendBuffer {
		seg := kcp.sendBuffer[idx]
		diff := timediff(seg.resendTs, currentTime)
		if diff <= 0 {
			return currentTime
		}
		if int(diff) < tmPacket {
			tmPacket = int(diff)
		}
	}

	if tmPacket < tmFlush {
		minimal = tmPacket
	} else {
		minimal = tmFlush
	}

	if minimal >= int(kcp.interval) {
		minimal = int(kcp.interval)
	}

	return currentTime + uint32(minimal)
}

func (kcp *KCP) SetNoDelay(noDelay, interval, resend int, nc bool) {
	if noDelay >= 0 {
		kcp.noDelay = true
		if noDelay > 0 {
			kcp.rxMinRTO = KCP_RTO_NDL
		} else {
			kcp.rxMinRTO = KCP_RTO_MIN
		}
	}

	kcp.SetMTU(interval)
	kcp.fastResendACK = uint32(resend)
	kcp.nocwnd = nc
}

func (kcp *KCP) SetWndSize(sendWnd, recvWnd int) {
	kcp.sendWnd = uint32(sendWnd)
	kcp.recvWnd = max(uint32(recvWnd), KCP_WND_RCV)
}

func (kcp *KCP) HowManyWait2Send() int {
	return len(kcp.sendQueue) + len(kcp.sendBuffer)
}

func (kcp *KCP) output(data []byte) {
	if len(data) <= 0 {
		return
	}

	kcp.outputCallback(data)
}
