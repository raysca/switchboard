// Command vadworker is the isolated Silero VAD worker process for production.
// It communicates with the parent process over stdin/stdout using framed binary IPC.
// stdout is strictly reserved for the binary protocol; stderr is for diagnostics.
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"govoice/vad"
)

func main() {
	maxBatch := 4
	if len(os.Args) > 1 {
		if b, err := strconv.Atoi(os.Args[1]); err == nil && b > 0 {
			maxBatch = b
		}
	}

	// 1. Verify model artifact SHA-256 before loading
	modelSHA, err := vad.VerifyModelArtifact(vad.ModelPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "vadworker: model verification failed: %v\n", err)
		os.Exit(1)
	}

	// 2. Initialize ONNX Runtime runner
	runner, err := vad.NewRunner(maxBatch, 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vadworker: init ORT runner failed: %v\n", err)
		os.Exit(2)
	}
	defer vad.ShutdownORT() // runs after runner.Close
	defer runner.Close()

	// 3. Complete handshake with parent
	hsPayload, err := vad.ReadFrame(os.Stdin, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vadworker: read handshake failed: %v\n", err)
		os.Exit(3)
	}

	hsReq, err := vad.DecodeHandshakeReq(hsPayload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vadworker: decode handshake failed: %v\n", err)
		os.Exit(4)
	}

	var hsResp vad.HandshakeResp
	hsResp.WorkerPID = uint32(os.Getpid())
	copy(hsResp.ActualModelSHA256[:], modelSHA[:])
	copy(hsResp.ORTVersion[:], []byte(vad.PinnedORTVersion))

	if hsReq.ExpectedModelSHA256 != modelSHA {
		hsResp.Status = vad.StatusModelMismatch
		_ = vad.WriteFrame(os.Stdout, vad.EncodeHandshakeResp(hsResp))
		fmt.Fprintf(os.Stderr, "vadworker: expected model SHA mismatch\n")
		os.Exit(5)
	}

	hsResp.Status = vad.StatusOK
	if err := vad.WriteFrame(os.Stdout, vad.EncodeHandshakeResp(hsResp)); err != nil {
		fmt.Fprintf(os.Stderr, "vadworker: write handshake response failed: %v\n", err)
		os.Exit(6)
	}

	// 4. Main request loop
	jobs := make([]*vad.Job, maxBatch)
	for i := range jobs {
		jobs[i] = &vad.Job{}
	}

	for {
		payload, err := vad.ReadFrame(os.Stdin, nil)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return // parent closed connection or exited
			}
			fmt.Fprintf(os.Stderr, "vadworker: read frame error: %v\n", err)
			return
		}

		reqItems, err := vad.DecodeInferReq(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vadworker: decode infer req error: %v\n", err)
			return
		}

		bSize := len(reqItems)
		batch := jobs[:bSize]
		for i, item := range reqItems {
			batch[i].Input = item.Input
			batch[i].State = item.State
		}

		runErr := runner.Run(batch)

		respItems := make([]vad.InferItemResp, bSize)
		for i, item := range reqItems {
			respItems[i].RequestID = item.RequestID
			respItems[i].SessionID = item.SessionID
			respItems[i].Seq = item.Seq
			if runErr != nil {
				respItems[i].Status = vad.StatusError
			} else {
				respItems[i].Status = vad.StatusOK
				respItems[i].Prob = batch[i].Prob
				respItems[i].State = batch[i].State
			}
		}

		respData := vad.EncodeInferResp(respItems)
		if err := vad.WriteFrame(os.Stdout, respData); err != nil {
			fmt.Fprintf(os.Stderr, "vadworker: write infer resp error: %v\n", err)
			return
		}
	}
}
