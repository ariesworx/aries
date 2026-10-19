// Package sextant is Sextant's exporter (docs/design/sextant.md, slice
// X0): a binary of its own, cmd/sextant, that reads the projects its
// config names from one ariesd through the public API, the ops `aries`
// itself sends, and hands what it read to the exporters of
// internal/telemetry, which push it over OTLP and offer it on a
// Prometheus endpoint. It changes nothing in Aries, never touches the
// database, and reads with its principal's rights, as a person would.
//
// It signs in by a key of its own, the only key it offers, or as a
// service principal with its platform's token (a workload's sign-in,
// kept in its data directory and renewed before it ends). It keeps a
// connection to each project between polls and makes a new one when it
// is lost. A poll that fails counts sextant.scrape.errors by project, op
// and code, and clears the project's figures until a poll reads them
// again, so a dashboard shows a gap rather than an old figure.
//
// What it exports is bounded (config.go's limits): the projects it is
// configured with, and the models with a price in the server's price
// table, capped; any other model is (other). `aries` links none of it:
// internal/depcheck's link sets hold each binary's modules apart.
package sextant
