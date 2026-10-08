# rivapb

Generated Go stubs for the NVIDIA Riva ASR and TTS gRPC services (messages and
clients only; no server code is used).

- Source: https://github.com/nvidia-riva/common, commit 268890b72860 (2026-08-13),
  files `riva/proto/riva_{common,audio,asr,tts}.proto`.
- License of the protos: MIT (SPDX header in each file; see the upstream LICENSE).
  `riva_asr.proto` also carries Google LLC's Apache-2.0 notice (derived from
  Google Cloud Speech); both headers are kept in the generated files.
- Generated with protoc + protoc-gen-go + protoc-gen-go-grpc v1.6.2:

```
protoc --proto_path=<common checkout> --go_out=. --go-grpc_out=. \
  --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative \
  --go_opt=Mriva/proto/riva_X.proto=govoice/providers/nvidia/rivapb\;rivapb \
  --go-grpc_opt=Mriva/proto/riva_X.proto=govoice/providers/nvidia/rivapb\;rivapb \
  riva/proto/riva_{common,audio,asr,tts}.proto      # M flags for X in common audio asr tts
```
Do not edit the `*.pb.go` files by hand.
