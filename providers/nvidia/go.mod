module govoice/providers/nvidia

go 1.27.1

require (
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.11
	govoice/core v0.0.0
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
)

replace govoice/core => ../../core
