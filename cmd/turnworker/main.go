// Command turnworker is the isolated Smart Turn v3 ONNX worker. The parent
// speaks the smartturn wire protocol over stdin/stdout; stderr is diagnostics.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"govoice/smartturn"
	"govoice/vad"
)

func main() {
	intraOp := 1
	if len(os.Args) > 1 {
		if n, err := strconv.Atoi(os.Args[1]); err == nil && n > 0 {
			intraOp = n
		}
	}
	// NewRunner hash-checks the model bytes it loads; the handshake then
	// reports the pinned hash it was verified against.
	r, err := smartturn.NewRunner(smartturn.ModelPath(), intraOp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "turnworker: init: %v\n", err)
		os.Exit(2)
	}
	defer vad.ShutdownORT() // runs after r.Close
	defer r.Close()
	var sha [32]byte
	_, _ = hex.Decode(sha[:], []byte(smartturn.PinnedModelSHA256))

	var want [32]byte
	if _, err := io.ReadFull(os.Stdin, want[:]); err != nil {
		os.Exit(3)
	}
	hs := make([]byte, 36)
	if want != sha {
		binary.LittleEndian.PutUint32(hs, 4)
	}
	copy(hs[4:], sha[:])
	if _, err := os.Stdout.Write(hs); err != nil || want != sha {
		os.Exit(4)
	}

	var hdr [4]byte
	for {
		if _, err := io.ReadFull(os.Stdin, hdr[:]); err != nil {
			return // parent closed
		}
		n := int(binary.LittleEndian.Uint32(hdr[:]))
		if n < 1 || n > smartturn.WindowLen {
			fmt.Fprintf(os.Stderr, "turnworker: bad sample count %d\n", n)
			os.Exit(5)
		}
		raw := make([]byte, 2*n)
		if _, err := io.ReadFull(os.Stdin, raw); err != nil {
			return
		}
		pcm := make([]int16, n)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
		}
		var resp [8]byte
		if p, err := r.Infer(pcm); err != nil {
			fmt.Fprintf(os.Stderr, "turnworker: infer: %v\n", err)
			binary.LittleEndian.PutUint32(resp[:4], 1)
		} else {
			binary.LittleEndian.PutUint32(resp[4:], math.Float32bits(p))
		}
		if _, err := os.Stdout.Write(resp[:]); err != nil {
			return
		}
	}
}
