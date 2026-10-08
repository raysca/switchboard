package main

import (
	"context"
	"fmt"
	"time"

	"govoice/providers/google"
)

// sttProbe streams a clip + trailing silence straight into Google streaming STT (one stream,
// no server) and prints when each interim/final result arrives. It shows what the
// turn manager's min_words gate and the VAD/STT timing have to work with (-stt-probe).
func sttProbe(ctx context.Context, name, model string, pcm, sa []byte) {
	cfg := google.DefaultSTTConfig()
	cfg.Model, cfg.LanguageCode, cfg.CredentialsJSON = model, "en-US", sa
	op, err := google.NewRealGoogleSTTOpener(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stdout, "stt probe:", redact(err))
		return
	}
	defer op.Close()
	st, err := op.OpenStream(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stdout, "stt probe:", redact(err))
		return
	}
	t0 := time.Now()
	go func() {
		in := append(append([]byte(nil), pcm...), pcmSilence(4*time.Second)...)
		next := time.Now()
		for off := 0; off < len(in); off += chunkBytes {
			time.Sleep(time.Until(next))
			next = next.Add(chunkMS * time.Millisecond)
			if st.Send(in[off:min(off+chunkBytes, len(in))]) != nil {
				return
			}
		}
		_ = st.CloseSend()
	}()
	fmt.Fprintf(stdout, "stt probe %s: speech ends at %.1fs\n", name, pcmDur(pcm).Seconds())
	for {
		rs, err := st.Recv()
		if err != nil {
			return
		}
		for _, r := range rs {
			fmt.Fprintf(stdout, "  %5.2fs final=%v %q\n", time.Since(t0).Seconds(), r.IsFinal, r.Transcript)
		}
	}
}
