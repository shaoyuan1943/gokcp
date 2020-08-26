package gokcp

import (
	"container/list"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"
)

type DelayPacket struct {
	data []byte
	Ts   uint32
}

func NewDelayPacket(data []byte) *DelayPacket {
	dp := &DelayPacket{
		data: nil,
		Ts:   0,
	}

	dp.data = make([]byte, len(data))
	copy(dp.data, data)
	return dp
}

type Random struct {
	seeds []int
	size  int
}

func (r *Random) Rand() int {
	if len(r.seeds) == 0 {
		return 0
	}

	if r.size == 0 {
		for i := 0; i < len(r.seeds); i++ {
			r.seeds[i] = i
		}

		r.size = len(r.seeds)
	}

	v := rand.Int() % r.size
	x := r.seeds[v]
	r.size -= 1
	r.seeds[v] = r.seeds[r.size]
	return x
}

func NewRandom(size int) *Random {
	r := &Random{}
	r.seeds = make([]int, size)
	return r
}

func TestRandom(t *testing.T) {
	rand.Seed(time.Now().UnixNano())
	randomValues := make([]int, 1000)
	random := NewRandom(100)
	for i := 0; i < 1000; i++ {
		randomValues[i] = random.Rand()
	}

	t.Logf("randomValues: %v", randomValues)
}

type LatencySimulator struct {
	lostRate int // 丢包率
	rttMin   int // 最小往返时间
	rttMax   int // 最大往返时间
	max      int
	rand12   *Random
	dt12     *list.List
	rand21   *Random
	dt21     *list.List
	TX1      int
	TX2      int
}

func NewLatencySimulator(lostRate, rttMin, rttMax, max int) *LatencySimulator {
	ls := &LatencySimulator{
		lostRate: lostRate / 2,
		rttMin:   rttMin / 2,
		rttMax:   rttMax / 2,
		max:      max,
	}

	ls.rand12 = NewRandom(100)
	ls.dt12 = list.New()

	ls.rand21 = NewRandom(100)
	ls.dt21 = list.New()
	return ls
}

// 发送数据
// peer - 端点0/1，从0发送，从1接收；从1发送从0接收
func (ls *LatencySimulator) Send(peer int, data []byte) {
	if peer == 0 {
		ls.TX1++
		if ls.rand12.Rand() < ls.lostRate {
			return
		}

		if ls.dt12.Len() >= ls.max {
			return
		}
	} else {
		ls.TX2++
		if ls.rand21.Rand() < ls.lostRate {
			return
		}

		if ls.dt21.Len() >= ls.max {
			return
		}
	}

	packet := NewDelayPacket(data)
	current := CurrentMS()
	delay := ls.rttMin
	if ls.rttMax > ls.rttMin {
		delay += rand.Int() % (ls.rttMax - ls.rttMin)
	}
	packet.Ts = current + uint32(delay)
	if peer == 0 {
		ls.dt12.PushBack(packet)
	} else {
		ls.dt21.PushBack(packet)
	}
}

func (ls *LatencySimulator) Recv(peer int, data []byte, max int) int {
	var ele *list.Element
	if peer == 0 {
		if ls.dt21.Len() == 0 {
			return -1
		} else {
			ele = ls.dt21.Front()
		}
	} else {
		if ls.dt12.Len() == 0 {
			return -1
		} else {
			ele = ls.dt12.Front()
		}
	}

	packet := ele.Value.(*DelayPacket)
	current := CurrentMS()
	if current < packet.Ts {
		return -2
	}

	if max < len(packet.data) {
		return -3
	}

	if peer == 0 {
		ls.dt21.Remove(ele)
	} else {
		ls.dt12.Remove(ele)
	}

	max = len(packet.data)
	data = data[:max]
	copy(data, packet.data)
	return max
}

var vnet *LatencySimulator

func testKCP(t *testing.T, mode int, modestr string) {
	vnet = NewLatencySimulator(10, 60, 125, 1000)
	kcp1 := NewKCP(0x11223344, func(p []byte) {
		vnet.Send(0, p)
		t.Logf("kcp1 output: %v", p)
	})

	kcp2 := NewKCP(0x11223344, func(p []byte) {
		vnet.Send(1, p)
		t.Logf("kcp2 output: %v", p)
	})

	current := CurrentMS()
	var slap uint32 = current + uint32(20)
	var index uint32
	var next uint32
	var summaryRTT uint32

	count := 0
	maxRTT := 0

	kcp1.SetWndSize(128, 128)
	kcp2.SetWndSize(128, 128)

	if mode == 0 {
		kcp1.SetNoDelay(false, 10, 0, false)
		kcp2.SetNoDelay(false, 10, 0, false)
	} else if mode == 1 {
		kcp1.SetNoDelay(false, 10, 0, true)
		kcp2.SetNoDelay(false, 10, 0, true)
	} else {
		kcp1.SetNoDelay(true, 10, 2, true)
		kcp2.SetNoDelay(true, 10, 2, true)

		kcp1.rxMinRTO = 10
		kcp1.fastResendACK = 1
	}

	kcp1Sended := 0
	loops := 0
	buffer := make([]byte, 2000)
	ts1 := CurrentMS()
	for {
		loops++
		<-time.After(1 * time.Millisecond)

		current = CurrentMS()
		kcp1.Update()
		kcp2.Update()

		// 每隔 20ms，kcp1发送数据
		for ; current >= slap; slap += 20 {
			binary.LittleEndian.PutUint32(buffer[0:4], uint32(index))
			binary.LittleEndian.PutUint32(buffer[4:8], uint32(current))
			kcp1.Send(buffer[0:8])
			index++
			kcp1Sended++
			break
		}
		t.Logf("kcp1Sended: %v", kcp1Sended)

		// 处理虚拟网络：检测是否有udp包从p1->p2
		for {
			hr := vnet.Recv(1, buffer, 2000)
			if hr < 0 {
				break
			}

			t.Logf("kcp2 recved,len: %v", hr)
			// 如果 p2收到udp，则作为下层协议输入到kcp2
			kcp2.Input(buffer[:hr])
		}

		// 处理虚拟网络：检测是否有udp包从p2->p1
		for {
			hr := vnet.Recv(0, buffer, 2000)
			if hr < 0 {
				break
			}

			t.Logf("kcp1 recved, len: %v", hr)
			// 如果 p1收到udp，则作为下层协议输入到kcp1
			kcp1.Input(buffer[:hr])
		}

		// kcp2接收到任何包都返回回去
		for {
			hr, err := kcp2.Recv(buffer)
			if err != nil {
				break
			}

			t.Logf("kcp2.Send: %v", hr)
			// 如果收到包就回射
			kcp2.Send(buffer[:hr])
		}

		// kcp1收到kcp2的回射数据
		for {
			hr, err := kcp1.Recv(buffer)
			if err != nil {
				break
			}
			t.Logf("kcp1 recved: %v", hr)

			sn := binary.LittleEndian.Uint32(buffer[0:4])
			ts := binary.LittleEndian.Uint32(buffer[4:8])
			rtt := current - ts
			if sn != next {
				// 如果收到的包不连续
				t.Logf("ERROR sn %v <-> %v", count, next)
				return
			}

			next++
			summaryRTT += rtt
			count++
			if rtt > uint32(maxRTT) {
				maxRTT = int(rtt)
			}
			t.Logf("[RECV] mode=%v, sn=%v, rtt=%v", mode, sn, rtt)
		}

		if next > 1 || loops > 5 {
			break
		}
	}

	ts1 = CurrentMS() - ts1
	t.Logf("%s mode result: %vms", modestr, ts1)
	t.Logf("avgrtt=%v maxrtt=%v tx=%v", int(summaryRTT)/count, maxRTT, vnet.TX1)
}

func TestKCP(t *testing.T) {
	rand.Seed(time.Now().UnixNano())
	testKCP(t, 0, "default")
	//testKCP(t, 1, "normal")
	//testKCP(t, 2, "fast")
}
