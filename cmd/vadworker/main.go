// Command vadworker is a Strategy C VAD worker process: stateless Silero
// inference over stdin/stdout. Request: u32 n, then n x (576 f32 input, 256 f32 state).
// Response: n x (1 f32 prob, 256 f32 state). Little-endian (host order).
package main

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"unsafe"

	"govoice/vad"
)

func main() {
	maxBatch, _ := strconv.Atoi(os.Args[1])
	r, err := vad.NewRunner(maxBatch, 1)
	if err != nil {
		panic(err)
	}
	in, out := bufio.NewReaderSize(os.Stdin, 1<<16), bufio.NewWriterSize(os.Stdout, 1<<16)
	jobs := make([]*vad.Job, maxBatch)
	for i := range jobs {
		jobs[i] = &vad.Job{}
	}
	b := func(p *float32, n int) []byte { return unsafe.Slice((*byte)(unsafe.Pointer(p)), n*4) }
	var n uint32
	for {
		if _, err := io.ReadFull(in, unsafe.Slice((*byte)(unsafe.Pointer(&n)), 4)); err != nil {
			return // parent closed
		}
		batch := jobs[:n]
		for _, j := range batch {
			io.ReadFull(in, b(&j.Input[0], vad.InputSamples))
			if _, err := io.ReadFull(in, b(&j.State[0], vad.StateLen)); err != nil {
				return
			}
		}
		if err := r.Run(batch); err != nil {
			panic(err)
		}
		for _, j := range batch {
			out.Write(b(&j.Prob, 1))
			out.Write(b(&j.State[0], vad.StateLen))
		}
		if out.Flush() != nil {
			return
		}
	}
}
