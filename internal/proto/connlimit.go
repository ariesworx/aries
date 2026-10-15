package proto

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FixConnLimit is the fix of a refusal at a principal's connection
// limit (ConnLimit): what the person whose key it is can do.
const FixConnLimit = "close some of your other aries sessions (each running aries mcp, aries watch or aries tui holds a connection), " +
	"or wait for them to end, and retry; or ask the server's operator to raise conns_per_principal under limits: in ariesd's config"

// connLimitText follows the principal in ConnLimit's message.
const connLimitText = " is at its connection limit ("

// ConnLimit refuses a connection of principal, which holds most
// connections already: the server's conns_per_principal. Its message is
// the one released servers send, which AtConnLimit reads; keep it.
func ConnLimit(principal string, most int) *Error {
	return Errf(CodeBusy, FixConnLimit, fmt.Sprintf("%s%s%d)", principal, connLimitText, most))
}

// AtConnLimit reports whether err is ConnLimit's refusal, from this
// server or a released one, whatever its fix. It reads the message: the
// server's own connection limit says "the server is at its connection
// limit", and "the server" is no principal.
func AtConnLimit(err error) bool {
	pe, ok := errors.AsType[*Error](err)
	if !ok || pe.Code != CodeBusy {
		return false
	}
	who, rest, ok := strings.Cut(pe.Message, connLimitText)
	if !ok || !principal(who) {
		return false
	}
	n, ok := strings.CutSuffix(rest, ")")
	if !ok {
		return false
	}
	_, err = strconv.ParseUint(n, 10, 31)
	return err == nil
}

// principal reports whether s is a principal's name: a lowercase letter,
// then up to 63 lowercase letters, digits, '.', '_' and '-' (the store's
// PrincipalPattern, which proto cannot import).
func principal(s string) bool {
	if s == "" || len(s) > 64 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, r := range s[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
