package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/telemetry"
)

// ListenerTLS is the HTTPS listener's TLS as the settings make it: the
// configuration, nil behind a proxy; the mode; and when the certificate
// served ends, the zero time when that is not known (ACME's).
type ListenerTLS struct {
	TLS      *tls.Config
	Mode     string
	NotAfter func() time.Time
}

// TLSConfig makes the listener's TLS from https: tls: (transport.md
// §4.2): a certificate read from files, and again whenever they change;
// ACME's (golang.org/x/crypto/acme/autocert), which answers the
// TLS-ALPN-01 challenge on the listener itself; or none, behind a proxy.
// TLS 1.2 at least, or 1.3, with Go's own cipher suites. It refuses
// files that cannot be served, a key file others may read, and an ACME
// cache others may read, each with its fix; log is told when files are
// read again, or cannot be, and tel counts it.
func (h HTTPSSettings) TLSConfig(log *slog.Logger, tel *telemetry.Telemetry) (ListenerTLS, error) {
	minVersion := uint16(tls.VersionTLS12)
	if h.TLS.MinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}
	none := func() time.Time { return time.Time{} }
	switch h.TLS.Mode {
	case TLSModeFiles:
		cf := &certFiles{cert: h.TLS.Files.Cert, key: h.TLS.Files.Key, log: log, tel: tel}
		if err := cf.load(); err != nil {
			return ListenerTLS{}, err
		}
		cfg := &tls.Config{MinVersion: minVersion, GetCertificate: cf.get}
		return ListenerTLS{TLS: cfg, Mode: TLSModeFiles, NotAfter: cf.end}, nil
	case TLSModeACME:
		a := h.TLS.ACME
		if err := privateDir(a.Cache); err != nil {
			return ListenerTLS{}, err
		}
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(a.Cache), HostPolicy: autocert.HostWhitelist(a.Domains...), Email: a.Email}
		if a.Directory != "" {
			m.Client = &acme.Client{DirectoryURL: a.Directory}
		}
		cfg := m.TLSConfig()
		cfg.MinVersion = minVersion
		return ListenerTLS{TLS: cfg, Mode: TLSModeACME, NotAfter: none}, nil
	case TLSModeProxy:
		return ListenerTLS{Mode: TLSModeProxy, NotAfter: none}, nil
	}
	return ListenerTLS{}, fmt.Errorf("https: tls: mode: %q is not acme, files or proxy; fix: set it to one of them", h.TLS.Mode)
}

// privateDir makes dir, mode 0700, if it is not there, and refuses one
// that group or others may enter: ACME's cache holds the account's key
// and the certificates' keys.
func privateDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("https: tls: acme: cache: %w; fix: create %s, mode 0700, owned by the user ariesd runs as", err, dir)
		}
		return nil
	case err != nil:
		return fmt.Errorf("https: tls: acme: cache: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("https: tls: acme: cache: %s is not a directory; fix: point cache: at a directory of its own", dir)
	case runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("https: tls: acme: cache: %s is mode %04o, open to group or others, and holds private keys; fix: chmod 700 %s", dir, fi.Mode().Perm(), dir)
	}
	return nil
}

// certFiles serves the certificate in a pair of files, and reads them
// again when either changes: a renewal is taken at the next handshake,
// with no restart. A pair that does not read is logged once and the
// certificate served before is kept.
type certFiles struct {
	cert, key string
	log       *slog.Logger
	tel       *telemetry.Telemetry

	mu       sync.Mutex
	cur      *tls.Certificate
	notAfter time.Time
	// read is the files' state when they were last read, and failed when
	// they last could not be.
	read, failed filesState
}

// filesState is two files' sizes and times, which change when either is
// written.
type filesState struct {
	certSize, keySize int64
	certTime, keyTime time.Time
}

// state is the files' filesState now.
func (c *certFiles) state() (filesState, error) {
	cfi, err := os.Stat(c.cert)
	if err != nil {
		return filesState{}, err
	}
	kfi, err := os.Stat(c.key)
	if err != nil {
		return filesState{}, err
	}
	return filesState{certSize: cfi.Size(), keySize: kfi.Size(), certTime: cfi.ModTime(), keyTime: kfi.ModTime()}, nil
}

// load reads the pair, at start: an error names its fix.
func (c *certFiles) load() error {
	st, err := c.state()
	if err != nil {
		return fmt.Errorf("https: tls: files: %w; fix: put the certificate and its key at cert: and key:, readable by the user ariesd runs as", err)
	}
	cert, end, err := c.readPair()
	if err != nil {
		return err
	}
	c.cur, c.notAfter, c.read = cert, end, st
	return nil
}

// readPair reads and checks the pair: a key others cannot read, a
// certificate chain, and a key that is its leaf's.
func (c *certFiles) readPair() (*tls.Certificate, time.Time, error) {
	if fi, err := os.Stat(c.key); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, time.Time{}, fmt.Errorf("https: tls: files: the key %s is mode %04o, readable by group or others; fix: chmod 600 %s", c.key, fi.Mode().Perm(), c.key)
	}
	certPEM, err := os.ReadFile(c.cert)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("https: tls: files: %w; fix: let the user ariesd runs as read it", err)
	}
	keyPEM, err := os.ReadFile(c.key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("https: tls: files: %w; fix: let the user ariesd runs as read it", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// Its text names neither key nor certificate.
		return nil, time.Time{}, fmt.Errorf("https: tls: files: %s and %s are not a certificate and its key in PEM: %w; fix: give the certificate chain, leaf first, and the leaf's key", c.cert, c.key, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("https: tls: files: %s: %w", c.cert, err)
	}
	pair.Leaf = leaf
	return &pair, leaf.NotAfter, nil
}

// get is the listener's GetCertificate.
func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := c.state()
	if err != nil || st == c.read || st == c.failed {
		return c.cur, nil
	}
	cert, end, err := c.readPair()
	if err != nil {
		c.failed = st
		c.tel.Count(telemetry.HTTPSCertReloads, telemetry.ResultFailed)
		c.log.Error("the TLS certificate's files changed and could not be read; the listener keeps the certificate it had", "event", logs.TLSCertReloadFailed, "err", err)
		return c.cur, nil
	}
	c.cur, c.notAfter, c.read, c.failed = cert, end, st, filesState{}
	c.tel.Count(telemetry.HTTPSCertReloads, telemetry.ResultOK)
	c.log.Info("the TLS certificate's files changed and were read again", "event", logs.TLSCertReloaded, "path", c.cert)
	return c.cur, nil
}

// end is when the certificate served ends.
func (c *certFiles) end() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notAfter
}
