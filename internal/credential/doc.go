// Package credential keeps a secret of aries's, such as the dispatcher's
// API key, in the system's own store (docs/design/dispatcher.md §11,
// decision 96). It is a permanent part, which Bearings reuses.
//
// # Where
//
// [Open] picks the store and [Store.Name] says which it is:
//
//   - macOS: the keychain, through security(1).
//   - Linux and other Unix systems: the freedesktop Secret Service
//     (GNOME Keyring, KWallet), through secret-tool(1), when it is
//     installed and a service answers; otherwise a file of its own for
//     each secret, SERVICE.NAME in the directory "credentials" of aries's
//     data directory ([offline.DataDir]). The file is 0600 and the
//     directory 0700; a file that is not a regular file, the user's own
//     and 0600, or a directory another may read or write, is refused,
//     with a fix, and never repaired.
//   - Windows: Credential Manager, which refuses every call with a fix
//     until it is built, behind the same interface.
//
// # Why commands
//
// No new dependency (rule 4), and releases are built without cgo, so
// neither Security.framework nor libsecret can be called. The keychain is
// therefore security(1), which every Mac has; the Secret Service is a
// D-Bus API that a client on the standard library alone would have to
// speak by hand (the protocol, its authentication and the service's
// session encryption), where secret-tool already does. Windows, when it
// is built, needs no command: advapi32's Cred* functions through syscall.
//
// # The secret
//
// It is printable ASCII with no space, 16 to 4096 bytes: an API key or a
// token, as its provider issued it. It is never in a command line: a
// command that writes it reads it from stdin (`security -i`, given the
// command with the secret in hexadecimal, and `secret-tool store`), and
// every command is started by one function, which refuses an argv that
// carries it. It is in no error: what a command prints on stderr is
// redacted before an error quotes it. Nothing here logs or writes to the
// process's streams.
//
// # No command yet
//
// Nothing in `aries` sets or reads a credential yet: the dispatcher's
// loop is this package's first caller, and its command (`aries dispatch`)
// is where a person will keep the key, read from stdin or a prompt and
// never an argument.
package credential
