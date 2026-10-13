// Package release finds, downloads and verifies starfix releases, and
// swaps a running binary for a new one.
//
// A release carries one archive per binary and platform ([ArchiveName]),
// a checksums.txt of their sha256 sums, and checksums.txt.sig, a signature
// over checksums.txt. [Client.Fetch] returns an executable only after its
// [Verifier] accepts that signature (in production, [Ed25519] with the
// keys built into this binary) and the archive's sha256 matches the one
// checksums.txt lists. Every download is capped in size. A Client's
// methods do not modify it, so one Client may serve several goroutines.
//
// Where a release's files are read from is a separate matter, and is
// never what a release is trusted on. A Client reads one of two sources,
// which [Open] chooses between: GitHub's API, where assets must be served
// over the API's own scheme; or a static tree, an [Index] at index.json
// and a directory for each tag, on any host that serves files over HTTPS
// or in a directory. A static host is asked and no other: no redirect is
// followed, a tag is checked ([ValidTag]) before it is put in a URL, and
// plain HTTP is accepted only to a loopback address. What a host can
// still do is serve an old index, and so keep a caller from learning of a
// newer release; that a caller never installs an older one is the
// caller's rule, and both upgrade commands keep it.
//
// [Swap] installs the new executable over the running one and keeps the
// one it replaced for [Swap.Rollback]. It works on files beside the
// executable and takes no lock, so callers run one swap at a time.
//
// The design is docs/design/starfix.md §11, whose As-built notes cover
// signing and the static source; RELEASING.md covers the release
// workflow, the static layout and key rotation.
package release
