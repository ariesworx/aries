package credential

import (
	"context"

	"github.com/ariesworx/aries/internal/proto"
)

// windows is Windows Credential Manager, not built yet: the dispatcher's
// loop does not run on Windows, and Bearings will need it. It is to be
// CredWriteW, CredReadW and CredDeleteW of advapi32.dll through
// syscall.NewLazyDLL, with a generic credential named SERVICE/NAME, so
// that the secret passes through no process at all. Until then each call
// refuses, saying so.
type windows struct{}

func (windows) Kind() Kind   { return KindWindows }
func (windows) Name() string { return "Windows Credential Manager" }

func (windows) refuse() error {
	return proto.Errf(proto.CodeUnavailable,
		"run this on macOS or Linux, where aries keeps credentials in the keychain, the Secret Service or a 0600 file",
		"aries does not yet keep credentials in Windows Credential Manager")
}

func (w windows) Get(context.Context, string) (string, bool, error) { return "", false, w.refuse() }
func (w windows) Set(context.Context, string, string) error         { return w.refuse() }
func (w windows) Remove(context.Context, string) error              { return w.refuse() }
