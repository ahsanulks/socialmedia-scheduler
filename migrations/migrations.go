package migrations

import "embed"

// FS holds the embedded schema migrations, applied in filename order on startup.
//
//go:embed *.sql
var FS embed.FS
