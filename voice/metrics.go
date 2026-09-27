package voice

import (
	"math"
	"math/bits"
	"sync/atomic"
	"time"
)

// Hist is a lock-free log-linear latency histogram (16 sub-buckets per power
// of two, ~4% resolution) over nanoseconds.
// ponytail: fixed buckets instead of HDR lib, resolution is plenty for p99 at ms scale.
type Hist struct {
	counts [histBuckets]atomic.Uint64
	n      atomic.Uint64
	sum    atomic.Uint64
	max    atomic.Uint64
}

const (
	histSub     = 16
	histBuckets = 40 * histSub
)

func bucketOf(v uint64) int {
	if v < histSub {
		return int(v)
	}
	e := bits.Len64(v) - 5 // exponent above sub-bucket bits
	i := (e+1)*histSub + int((v>>uint(e))&(histSub-1))
	if i >= histBuckets {
		return histBuckets - 1
	}
	return i
}

// bucketUpper returns the upper bound (ns) of bucket i.
func bucketUpper(i int) uint64 {
	if i < histSub {
		return uint64(i)
	}
	e := i/histSub - 1
	m := uint64(i%histSub) | histSub
	return (m+1)<<uint(e) - 1
}

func (h *Hist) Record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	v := uint64(d)
	h.counts[bucketOf(v)].Add(1)
	h.n.Add(1)
	h.sum.Add(v)
	for {
		m := h.max.Load()
		if v <= m || h.max.CompareAndSwap(m, v) {
			return
		}
	}
}

// Snap is an immutable copy of a histogram, used for windowed percentiles.
type Snap struct {
	Counts [histBuckets]uint64
	N, Sum uint64
	Max    uint64
}

func (h *Hist) Snapshot() Snap {
	var s Snap
	for i := range h.counts {
		s.Counts[i] = h.counts[i].Load()
	}
	s.N, s.Sum, s.Max = h.n.Load(), h.sum.Load(), h.max.Load()
	return s
}

// Sub returns s - prev (window between two snapshots). Max is not windowable; it keeps s.Max.
func (s Snap) Sub(prev Snap) Snap {
	for i := range s.Counts {
		s.Counts[i] -= prev.Counts[i]
	}
	s.N -= prev.N
	s.Sum -= prev.Sum
	return s
}

func (s *Snap) Add(o *Snap) {
	for i := range s.Counts {
		s.Counts[i] += o.Counts[i]
	}
	s.N += o.N
	s.Sum += o.Sum
	s.Max = max(s.Max, o.Max)
}

func (s *Snap) Quantile(q float64) time.Duration {
	if s.N == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(s.N)))
	var c uint64
	for i, v := range s.Counts {
		c += v
		if c >= target {
			return time.Duration(bucketUpper(i))
		}
	}
	return time.Duration(s.Max)
}

func (s *Snap) Mean() time.Duration {
	if s.N == 0 {
		return 0
	}
	return time.Duration(s.Sum / s.N)
}

func (h *Hist) Quantile(q float64) time.Duration { s := h.Snapshot(); return s.Quantile(q) }
func (h *Hist) Count() uint64                    { return h.n.Load() }
