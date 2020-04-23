package gokcp

type Stats struct {
	RTOs []int32
}

func newStats() *Stats {
	s := &Stats{}
	return s
}
