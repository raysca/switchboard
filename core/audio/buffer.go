package audio

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
)

type OverflowPolicy uint8

const (
	OverflowDropOldest OverflowPolicy = iota
	OverflowRejectNewest
)

func checkBound(capacity int, policy OverflowPolicy) error {
	if capacity <= 0 {
		return errors.New("audio: capacity must be positive")
	}
	if policy != OverflowDropOldest && policy != OverflowRejectNewest {
		return errors.New("audio: invalid overflow policy")
	}
	return nil
}

// PreRoll retains the newest bytes before speech starts. Returned snapshots do
// not alias its storage. All operations are safe to call concurrently.
type PreRoll struct {
	mu       sync.Mutex
	capacity int
	policy   OverflowPolicy
	data     []byte
}

func NewPreRoll(capacity int, policy OverflowPolicy) (*PreRoll, error) {
	if err := checkBound(capacity, policy); err != nil {
		return nil, err
	}
	return &PreRoll{capacity: capacity, policy: policy}, nil
}
func (p *PreRoll) Push(input []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.policy == OverflowRejectNewest && len(p.data)+len(input) > p.capacity {
		return errors.New("audio: pre-roll overflow")
	}
	if len(input) >= p.capacity {
		p.data = append(p.data[:0], input[len(input)-p.capacity:]...)
		return nil
	}
	over := len(p.data) + len(input) - p.capacity
	if over > 0 {
		copy(p.data, p.data[over:])
		p.data = p.data[:len(p.data)-over]
	}
	p.data = append(p.data, input...)
	return nil
}
func (p *PreRoll) Bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.data...)
}
func (p *PreRoll) Reset() { p.mu.Lock(); defer p.mu.Unlock(); p.data = nil }

// Rechunker emits fixed-size byte chunks while holding at most capacity bytes.
type Rechunker struct {
	mu             sync.Mutex
	size, capacity int
	policy         OverflowPolicy
	data           []byte
}

func NewRechunker(size, capacity int, policy OverflowPolicy) (*Rechunker, error) {
	if err := checkBound(capacity, policy); err != nil {
		return nil, err
	}
	if size <= 0 || size > capacity {
		return nil, errors.New("audio: invalid chunk size")
	}
	return &Rechunker{size: size, capacity: capacity, policy: policy}, nil
}
func (r *Rechunker) Push(input []byte) ([][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.data)+len(input) > r.capacity {
		if r.policy == OverflowRejectNewest {
			return nil, errors.New("audio: rechunker overflow")
		}
		if len(input) >= r.capacity {
			r.data = append(r.data[:0], input[len(input)-r.capacity:]...)
			input = nil
		} else {
			drop := len(r.data) + len(input) - r.capacity
			copy(r.data, r.data[drop:])
			r.data = r.data[:len(r.data)-drop]
		}
	}
	r.data = append(r.data, input...)
	var out [][]byte
	for len(r.data) >= r.size {
		out = append(out, bytes.Clone(r.data[:r.size]))
		r.data = r.data[r.size:]
	}
	return out, nil
}
func (r *Rechunker) Flush() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := bytes.Clone(r.data)
	r.data = nil
	return out
}
func (r *Rechunker) Reset() { r.mu.Lock(); defer r.mu.Unlock(); r.data = nil }

// TurnBuffer captures only audio admitted between Start and matching End.
// Disabled buffers retain no content. It is memory-only and stream-local.
type TurnBuffer struct {
	mu       sync.Mutex
	enabled  bool
	capacity int
	active   bool
	id       string
	data     []byte
}

func NewTurnBuffer(enabled bool, capacity int) (*TurnBuffer, error) {
	if capacity <= 0 {
		return nil, errors.New("audio: capacity must be positive")
	}
	return &TurnBuffer{enabled: enabled, capacity: capacity}, nil
}
func (b *TurnBuffer) Start(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.id = id
	b.active = true
	b.data = nil
}
func (b *TurnBuffer) Push(input []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled || !b.active {
		return
	}
	if len(input) >= b.capacity {
		b.data = append(b.data[:0], input[len(input)-b.capacity:]...)
		return
	}
	over := len(b.data) + len(input) - b.capacity
	if over > 0 {
		copy(b.data, b.data[over:])
		b.data = b.data[:len(b.data)-over]
	}
	b.data = append(b.data, input...)
}
func (b *TurnBuffer) End(id string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.active || b.id != id {
		return nil, fmt.Errorf("audio: no active turn %q", id)
	}
	out := bytes.Clone(b.data)
	b.active = false
	b.id = ""
	b.data = nil
	return out, nil
}
func (b *TurnBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active = false
	b.id = ""
	b.data = nil
}
