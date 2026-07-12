// Package migrations embeds the StateFlow schema migrations and applies
// them via golang-migrate's Go library (never its CLI — the distroless
// runtime image (Dockerfile) has no shell to run a CLI in). Temporary
// Design Registry item #7, whitepaper §18.
//
// Migration files follow golang-migrate's numbered up/down naming
// convention: NNNNNN_title.up.sql / NNNNNN_title.down.sql. 000001_initial
// is a mechanical split of the former single-file migrations/001_initial.sql
// — the up file's content is unchanged, byte-for-byte, from that file.
package migrations

import "embed"

//go:embed *.sql
var fs embed.FS
