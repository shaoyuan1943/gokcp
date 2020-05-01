package gokcp

type Stats struct {
	RTOs            []int32
	SendedACK       int64
	SendedDataCount int64
	FlushTimes      int64
	DataInputTimes  int64
}

func newStats() *Stats {
	s := &Stats{}
	return s
}
