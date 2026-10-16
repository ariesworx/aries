package client

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// The transports aries reaches a server by (transport.md §5.7).
const (
	TransportHTTPS = "https"
	TransportSSH   = "ssh"
)

// EnvTransport is the variable that chooses the transport where
// .aries.yaml names the server for both: ssh, say, to go through IAP.
// It is the person's, never the committed file's.
const EnvTransport = "ARIES_TRANSPORT"

// Transport returns the transport the server is reached by: getenv's
// EnvTransport when it is set, else HTTPS when server.url is set, and
// SSH otherwise. A transport the config does not name the server for
// is refused, with what to set.
func (c *Config) Transport(getenv func(string) string) (string, error) {
	switch v := getenv(EnvTransport); v {
	case "":
		if c.Server.URL != "" {
			return TransportHTTPS, nil
		}
		return TransportSSH, nil
	case TransportHTTPS:
		if c.Server.URL == "" {
			return "", proto.Errf(proto.CodeInvalid, fmt.Sprintf("set server.url in %s to the server's https:// URL, or unset %s", ConfigFile, EnvTransport),
				fmt.Sprintf("%s=https, and %s names no server.url", EnvTransport, ConfigFile))
		}
		return TransportHTTPS, nil
	case TransportSSH:
		if !c.sshNamed() {
			return "", proto.Errf(proto.CodeInvalid, fmt.Sprintf("set server.host and server.host_key in %s, or unset %s to use server.url", ConfigFile, EnvTransport),
				fmt.Sprintf("%s=ssh, and %s names no server.host for SSH", EnvTransport, ConfigFile))
		}
		return TransportSSH, nil
	default:
		return "", proto.Errf(proto.CodeInvalid, fmt.Sprintf("set %s to ssh or https, or unset it", EnvTransport),
			fmt.Sprintf("%s=%q is not ssh or https", EnvTransport, v))
	}
}

// ServerIDs names the server as what is kept on this machine for it is
// keyed (internal/offline): for HTTPS its URL, and for SSH the host keys
// pinned. A server reached both ways is two servers to the outbox, so
// that what was queued is sent by the transport that queued it.
func (c *Config) ServerIDs(getenv func(string) string) []string {
	t, err := c.Transport(getenv)
	if t == TransportHTTPS || err != nil && c.Server.URL != "" {
		return []string{c.Server.URL}
	}
	return c.Pinned()
}

// validateCA checks server.ca: a CA's certificate in PEM, each block a
// certificate, or a path that stays inside the repository, which is
// read when aries connects ([Config.Roots]).
func (c *Config) validateCA() error {
	ca := strings.TrimSpace(c.Server.CA)
	switch {
	case ca == "":
		return nil
	case strings.HasPrefix(ca, "-----BEGIN"):
		_, err := parseCerts([]byte(ca))
		return err
	case strings.ContainsAny(ca, "\r\n\x00"), filepath.IsAbs(ca), ca == "~", strings.HasPrefix(ca, "~/"), filepath.VolumeName(ca) != "",
		ca == "..", strings.HasPrefix(filepath.ToSlash(filepath.Clean(ca)), "../"):
		return errors.New("server.ca must be a CA's certificate in PEM, or the path of a PEM file inside the repository, relative to its root")
	}
	return nil
}

// parseCerts reads every certificate of a PEM text, and refuses one that
// holds anything else or none.
func parseCerts(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("server.ca holds a %s block; it holds a CA's certificate, never a key", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("server.ca holds a certificate that does not parse: %w", err)
		}
		out = append(out, cert)
	}
	if len(out) == 0 || strings.TrimSpace(string(b)) != "" {
		return nil, errors.New("server.ca is not a CA's certificate in PEM")
	}
	return out, nil
}

// Roots returns the system's roots with server.ca's added, or nil, the
// system's own, when the config names no CA. A file is read from inside
// the repository: with symbolic links resolved it must stay under Root,
// as the key file must ([Config.KeyPath]).
func (c *Config) Roots() (*x509.CertPool, error) {
	ca := strings.TrimSpace(c.Server.CA)
	if ca == "" {
		return nil, nil
	}
	b := []byte(ca)
	if !strings.HasPrefix(ca, "-----BEGIN") {
		path := filepath.Join(c.Root, ca)
		root, err := filepath.EvalSymlinks(c.Root)
		if err != nil {
			return nil, fmt.Errorf("server.ca: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, proto.Errf(proto.CodeInvalid, "put the CA's certificate there, or its PEM under server.ca itself",
				fmt.Sprintf("server.ca names %s, which cannot be read: %v", ca, errors.Unwrap(err)))
		}
		if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, proto.Errf(proto.CodeInvalid, "put the CA's certificate inside the repository, or its PEM under server.ca itself",
				fmt.Sprintf("server.ca names %s, which leads outside the repository", ca))
		}
		if b, err = os.ReadFile(resolved); err != nil { //nolint:gosec // inside the repository, checked above
			return nil, fmt.Errorf("server.ca: %w", err)
		}
	}
	certs, err := parseCerts(b)
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid, "give the CA's certificate in PEM: ask the server's operator for it", err.Error())
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool, nil
}
