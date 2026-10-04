package migrations

import "embed"

//go:embed sqlite/*.sql postgres/*.sql
var Files embed.FS
