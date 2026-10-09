package server

// Version and Commit are stamped at build time:
//
//	-ldflags "-X govoice/server.Version=v1.2.3 -X govoice/server.Commit=<sha>"
//
// They are exported as the voice_build_info metric and logged at startup.
var (
	Version = "dev"
	Commit  = "unknown"
)
