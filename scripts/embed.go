// Package scripts embeds the SQL artefacts that ship with the backend.
package scripts

import "embed"

// InitSQL is the authoritative schema + non-user seed script. store.Migrate executes it on every boot; it is idempotent.
//
//go:embed init.sql
var InitSQL string

// Migrations contains ordered, additive upgrades for existing installations.
//
//go:embed migrations/*.sql
var Migrations embed.FS
