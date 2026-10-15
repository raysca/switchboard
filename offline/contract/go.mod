module govoice/offline/contract

go 1.27.1

replace govoice/core => ../../core

replace govoice/providers/xai => ../../providers/xai

replace govoice/offline => ../

require (
	govoice/core v0.0.0
	govoice/offline v0.0.0
	govoice/providers/xai v0.0.0
)

require github.com/coder/websocket v1.8.15 // indirect
