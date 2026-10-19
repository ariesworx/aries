package credential

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// The fake secret-tool is the test binary, started with these variables
// (TestMain). It keeps each secret as a file named by its attributes in
// fakeDirEnv's directory, and behaves as secret-tool does when it can
// reach a Secret Service: store reads the secret from stdin, lookup
// prints it with no newline when stdout is not a terminal and exits 1
// with nothing printed when there is none, and clear exits 0 either way.
const (
	fakeEnv     = "ARIES_CREDENTIAL_FAKE"
	fakeDirEnv  = "ARIES_CREDENTIAL_FAKE_DIR"
	fakeModeEnv = "ARIES_CREDENTIAL_FAKE_MODE"
)

func fakeSecretTool(args []string) int {
	switch os.Getenv(fakeModeEnv) {
	case "absent":
		fmt.Fprintln(os.Stderr, "secret-tool: The name org.freedesktop.secrets was not provided by any .service files") //nolint:forbidigo // the fake is a process of its own
		return 1
	case "hang":
		time.Sleep(time.Hour)
		return 1
	case "echo":
		b, _ := io.ReadAll(os.Stdin)
		fmt.Fprintf(os.Stderr, "secret-tool: failed with %s %q\n", args, b) //nolint:forbidigo // the fake is a process of its own
		return 1
	}
	if len(args) < 1 {
		return 2
	}
	verb, attrs := args[0], args[1:]
	if verb == "store" {
		if len(attrs) == 0 || len(attrs[0]) < len("--label=") || attrs[0][:len("--label=")] != "--label=" {
			return 2
		}
		attrs = attrs[1:]
	}
	if len(attrs) != 4 || attrs[0] != "service" || attrs[2] != "account" {
		return 2
	}
	path := filepath.Join(os.Getenv(fakeDirEnv), attrs[1]+"."+attrs[3])
	switch verb {
	case "store":
		b, err := io.ReadAll(os.Stdin)
		if err != nil || os.WriteFile(path, b, 0o600) != nil { //nolint:gosec // the fake's own directory
			return 1
		}
	case "lookup":
		b, err := os.ReadFile(path) //nolint:gosec // the fake's own directory
		if err != nil {
			return 1
		}
		_, _ = os.Stdout.Write(b) //nolint:forbidigo // the fake is a process of its own
	case "clear":
		_ = os.Remove(path) //nolint:gosec // the fake's own directory
	default:
		return 2
	}
	return 0
}
