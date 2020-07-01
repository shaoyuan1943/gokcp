package gokcp

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/reedsolomon"
)

const (
	fecCmdData     = 0x0F
	fecCmdParity   = 0x0E
	fecHeaderSize  = 6
	fecResultSize  = 50
	fecDataTimeout = 10000
)

var (
	ErrUnknowFecCmd   = errors.New("unknow fec cmd")
	ErrFecDataTimeout = errors.New("fec data timeout")
)

type Encoder interface {
	Encode(rawData []byte) (fecData [][]byte, err error)
}

type Decoder interface {
	Decode(fecData []byte, ts uint32) (rawData [][]byte, err error)
}

type FecEncoder struct {
	codec         reedsolomon.Encoder
	q             [][]byte
	insertIndex   int
	nextSN        int32
	shards        int
	dataShards    int
	parityShards  int
	maxRawDataLen int
	zero          []byte
	codecData     [][]byte
	offset        int
}

func NewFecEncoder(dataShards, parityShards, headerOffset int) Encoder {
	fecEncoder := &FecEncoder{}
	fecEncoder.shards = dataShards + parityShards
	fecEncoder.dataShards = dataShards
	fecEncoder.parityShards = parityShards
	fecEncoder.offset = headerOffset
	encoder, err := reedsolomon.New(int(fecEncoder.dataShards), int(fecEncoder.parityShards))
	if err != nil {
		panic(fmt.Sprintf("init fec encoder: %v", err))
	}

	fecEncoder.codec = encoder
	fecEncoder.codecData = make([][]byte, fecEncoder.shards)
	fecEncoder.zero = make([]byte, KCP_MTU_DEF)
	fecEncoder.q = make([][]byte, fecEncoder.shards)
	for i := 0; i < fecEncoder.shards; i++ {
		fecEncoder.q[i] = make([]byte, KCP_MTU_DEF)
	}

	return fecEncoder
}

func (f *FecEncoder) HeaderOffset() int {
	return f.offset + fecHeaderSize
}

func (f *FecEncoder) Encode(rawData []byte) (fecData [][]byte, err error) {
	if rawData == nil || len(rawData) == 0 || len(rawData) > int(KCP_MTU_DEF) {
		panic("raw data length invalid")
	}

	l := len(rawData)
	f.q[f.insertIndex] = f.q[f.insertIndex][:l]
	copy(f.q[f.insertIndex], rawData)

	if l > f.maxRawDataLen {
		f.maxRawDataLen = l
	}

	if (f.insertIndex + 1) == int(f.dataShards) {
		for i := 0; i < (f.dataShards + f.parityShards); i++ {
			if i >= f.dataShards {
				f.q[i] = f.q[i][:f.maxRawDataLen]
				f.codecData[i] = f.q[i][f.HeaderOffset():f.maxRawDataLen]
			} else {
				orgLen := len(f.q[i])
				if orgLen < f.maxRawDataLen {
					f.q[i] = f.q[i][:f.maxRawDataLen]
					copy(f.q[i][orgLen:f.maxRawDataLen], f.zero)
				}

				f.codecData[i] = f.q[i][f.HeaderOffset():f.maxRawDataLen]
			}
		}

		err = f.codec.Encode(f.codecData)
		if err != nil {
			return
		}

		for i := 0; i < (f.dataShards + f.parityShards); i++ {
			if i >= f.dataShards {
				f.markParity(f.q[i])
			} else {
				f.markData(f.q[i])
			}
		}

		f.insertIndex = 0
		f.maxRawDataLen = 0

		fecData = f.q
		err = nil
		return
	}

	f.insertIndex++
	return
}

func (f *FecEncoder) markData(data []byte) {
	binary.LittleEndian.PutUint32(data[:4], uint32(f.nextSN))
	binary.LittleEndian.PutUint16(data[4:fecHeaderSize], uint16(fecCmdData))
	f.nextSN++
}

func (f *FecEncoder) markParity(data []byte) {
	binary.LittleEndian.PutUint32(data[:4], uint32(f.nextSN))
	binary.LittleEndian.PutUint16(data[4:fecHeaderSize], uint16(fecCmdParity))
	f.nextSN++
}

type DataShards struct {
	q             [][]byte
	o             [][]byte
	lastInsert    uint32
	insertIndex   int
	decoded       bool
	maxRawDataLen int
	shardsCount   int
}

type FecDecoder struct {
	codec        reedsolomon.Encoder
	shards       int
	dataShards   int
	parityShards int
	rawDatas     map[int]DataShards
	result       [][]byte
	offset       int
}

func NewFecDecoder(dataShards, parityShards, headerOffset int) Decoder {
	fecDecoder := &FecDecoder{}
	fecDecoder.shards = dataShards + parityShards
	fecDecoder.dataShards = dataShards
	fecDecoder.parityShards = parityShards
	decoder, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil
	}

	fecDecoder.codec = decoder
	fecDecoder.rawDatas = make(map[int]DataShards)
	fecDecoder.result = make([][]byte, fecResultSize)
	fecDecoder.offset = headerOffset
	return fecDecoder
}

func (f *FecDecoder) HeaderOffset() int {
	return f.offset + fecHeaderSize
}

func (f *FecDecoder) Decode(fecData []byte, now uint32) (rawData [][]byte, err error) {
	if fecData == nil || len(fecData) == 0 || len(fecData) > int(KCP_MTU_DEF) {
		panic("raw data length invalid")
	}

	fecCmd := binary.LittleEndian.Uint16(fecData[4:])
	if int(fecCmd) != fecCmdData && int(fecCmd) != fecCmdParity {
		return
	}

	sn := binary.LittleEndian.Uint32(fecData)
	startRange := int(sn) - (int(sn) % f.shards)
	endRange := startRange + f.shards + 1
	sumIndex := 0
	for i := startRange; i < endRange; i++ {
		sumIndex += i
	}

	ds, ok := f.rawDatas[sumIndex]
	if !ok {
		ds = DataShards{}
		ds.o = make([][]byte, f.shards)
		ds.q = make([][]byte, f.shards)
		for i := 0; i < len(ds.q); i++ {
			ds.o[i] = bufferPool.Get().([]byte)[:0]
			ds.q[i] = ds.o[i]
		}
	}

	ds.q[int(sn)-startRange] = ds.q[int(sn)-startRange][:len(fecData)]
	copy(ds.q[int(sn)-startRange], fecData)
	ds.shardsCount++

	if len(fecData) > ds.maxRawDataLen {
		ds.maxRawDataLen = len(fecData)
	}

	ds.lastInsert = now
	f.rawDatas[sumIndex] = ds

	f.result = f.result[:0]
	for idx, v := range f.rawDatas {
		if v.decoded {
			f.delShards(idx)
			continue
		}

		if len(f.result) >= fecResultSize || (len(f.result)+f.dataShards) >= fecResultSize {
			break
		}

		if v.shardsCount >= f.dataShards {
			codec := v.q
			for i := 0; i < len(v.q); i++ {
				d := v.q[i]
				if len(d) > 0 {
					sn = binary.LittleEndian.Uint32(d)
					cmd := int(binary.LittleEndian.Uint16(d[4:]))
					startRange = int(sn) - (int(sn) % f.shards)
					if cmd == fecCmdData {
						codec[(int(sn) - startRange)] = d[f.HeaderOffset():len(d)]
					} else if cmd == fecCmdParity {
						codec[(int(sn) - startRange)] = d[f.HeaderOffset():len(d)]
					} else {
						err = ErrUnknowFecCmd
						return
					}
				}
			}

			err = f.codec.ReconstructData(codec)
			if err != nil {
				return
			}

			v.decoded = true
			f.result = append(f.result, codec[:f.dataShards]...)
			f.rawDatas[idx] = v
			continue
		}

		// timeout
		if (now-v.lastInsert) > fecDataTimeout && !v.decoded {
			f.delShards(idx)
			err = ErrFecDataTimeout
			return
		}
	}

	rawData = f.result
	err = nil
	return
}

func (f *FecDecoder) delShards(sumIndex int) {
	ds, ok := f.rawDatas[sumIndex]
	if !ok {
		return
	}

	for i := 0; i < f.shards; i++ {
		if ds.o[i] != nil {
			bufferPool.Put(ds.o[i])
		}
	}

	ds.q = nil
	ds.o = nil
	delete(f.rawDatas, sumIndex)
}
