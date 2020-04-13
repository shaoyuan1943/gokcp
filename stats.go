package gokcp

type Stats struct {
	RTOs []uint32
}

func newStats() *Stats {
	s := &Stats{}
	return s
}
