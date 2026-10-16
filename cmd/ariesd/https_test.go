package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/wsconn"
)

// tlsFiles writes a CA's certificate for aries.example.com and
// 127.0.0.1, and its key (0600), to files, and returns their paths and
// a pool that trusts the CA.
func tlsFiles(t *testing.T) (cert, key string, pool *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "aries.example.com"},
		DNSNames: []string{"aries.example.com"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(caCert)
	return cert, key, pool
}

// httpsBlock is an https: setting with the certificate in files, on a
// port of loopback the system chooses, for clients that use
// https://aries.example.com.
func httpsBlock(cert, key string) string {
	return fmt.Sprintf("https:\n  listen: 127.0.0.1:0\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: files\n    files:\n      cert: %s\n      key: %s\n", cert, key)
}

// listening is the address the daemon's log says its HTTPS listener
// took.
func listening(t *testing.T, d *running) string {
	t.Helper()
	re := regexp.MustCompile(`event=https.listening addr=(\S+)`)
	var addr string
	waitFor(t, "the HTTPS listener", func() bool {
		m := re.FindStringSubmatch(d.log.String())
		if m != nil {
			addr = m[1]
		}
		return m != nil
	})
	return addr
}

// signerOf is the signer of pubKey's key for seed.
func signerOf(t *testing.T, seed string) ssh.Signer {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	s, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// connectWith opens /v1/connect at addr as a client of
// https://aries.example.com, offers signer's key, and returns the
// welcome to a hello: or the refusal of the key, which comes in its
// place.
func connectWith(t *testing.T, addr string, pool *x509.CertPool, signer ssh.Signer) *proto.Frame {
	t.Helper()
	const url = "https://aries.example.com"
	c, _, err := wsconn.Dial(t.Context(), "wss://"+addr+keyproof.ConnectPath, &tls.Config{RootCAs: pool, ServerName: "aries.example.com", MinVersion: tls.VersionTLS12}, nil, 0)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	if err := c.Write(keyproof.NewOffer(url, []ssh.PublicKey{signer.PublicKey()})); err != nil {
		t.Fatal(err)
	}
	m, err := c.Read()
	if err != nil {
		t.Fatalf("the answer to the key offer: %v", err)
	}
	ch, err := keyproof.ParseChallenge(m)
	if err != nil {
		var f proto.Frame
		if json.Unmarshal(m, &f) != nil {
			t.Fatalf("the answer to the key offer: %q", m)
		}
		return &f
	}
	sig, err := keyproof.Sign(signer, url, ch.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(keyproof.NewProof(sig)); err != nil {
		t.Fatal(err)
	}
	s := wsconn.Stream(c)
	if err := proto.NewEncoder(s).Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: adopted, Session: "s", Machine: "m"}); err != nil {
		t.Fatal(err)
	}
	f, err := proto.NewDecoder(s).Decode()
	if err != nil {
		t.Fatalf("the welcome: %v", err)
	}
	return f
}

// With no transports: in its config, serve serves HTTPS alone, and
// refuses to start until https: says where and how.
func TestServeNeedsHTTPS(t *testing.T) {
	t.Parallel()
	s := newSite(t, "# transports: left to its default\n")
	set := s.settings(t)
	if len(set.Transports) != 1 || set.Transports[0] != server.TransportHTTPS {
		t.Fatalf("the default transports = %q, want [https]", set.Transports)
	}
	err := runDaemon(t.Context(), set, slog.New(slog.DiscardHandler), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "https:") || !strings.Contains(err.Error(), "transports: [ssh]") {
		t.Errorf("serve with no https: = %v, want a refusal naming https: and transports: [ssh]", err)
	}
}

// A default install (transport.md §11.1, §12.4 item 10) exposes no SSH
// path and no path that skips authentication: the HTTPS listener serves
// its metadata and the WebSocket, whose protocol starts only once a key
// the server knows has proved itself; the bridge is refused; and no
// keys file is written for sshd to read.
func TestDefaultInstallExposes(t *testing.T) {
	t.Parallel()
	cert, key, pool := tlsFiles(t)
	s := newSite(t, "# transports: left to its default\n"+httpsBlock(cert, key))
	keys := writeKeys(t, forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	d := runSite(t, s, nil)
	addr := listening(t, d)

	t.Run("paths", func(t *testing.T) {
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "aries.example.com", MinVersion: tls.VersionTLS12}}
		t.Cleanup(tr.CloseIdleConnections)
		client := &http.Client{Transport: tr}
		for _, tc := range []struct {
			path   string
			status int
		}{
			{"/.well-known/aries", http.StatusOK}, {keyproof.ConnectPath, http.StatusBadRequest},
			{"/", http.StatusNotFound}, {"/metrics", http.StatusNotFound}, {"/healthz", http.StatusNotFound},
			{"/readyz", http.StatusNotFound}, {"/status", http.StatusNotFound}, {"/v1/p/" + adopted + "/connect", http.StatusNotFound},
			{"/mcp/p/" + adopted, http.StatusNotFound}, {"/oauth/token", http.StatusNotFound},
		} {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+tc.path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.status)
			}
			if tc.path == "/.well-known/aries" && !strings.Contains(string(body), `"transports":["https"]`) {
				t.Errorf("GET /.well-known/aries = %s, want transports [https] alone", body)
			}
		}
	})
	t.Run("the protocol before a key proof", func(t *testing.T) {
		c, _, err := wsconn.Dial(t.Context(), "wss://"+addr+keyproof.ConnectPath, &tls.Config{RootCAs: pool, ServerName: "aries.example.com", MinVersion: tls.VersionTLS12}, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		s := wsconn.Stream(c)
		_ = proto.NewEncoder(s).Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: adopted, Principal: "dana"})
		_ = proto.NewEncoder(s).Encode(&proto.Frame{T: proto.FrameReq, ID: 1, Op: proto.OpProjects, Args: json.RawMessage(`{}`)})
		_ = c.SetReadDeadline(time.Now().Add(time.Minute))
		f, err := proto.NewDecoder(s).Decode()
		if err != nil || f.T != proto.FrameWelcome || f.Err == nil || f.Principal != "" {
			t.Fatalf("a hello and a request in place of the key proof = %+v, %v; want a refusal that names no one", f, err)
		}
		if f, err := proto.NewDecoder(s).Decode(); err == nil {
			t.Errorf("the request was answered: %+v", f)
		}
	})
	t.Run("an unknown key", func(t *testing.T) {
		if f := connectWith(t, addr, pool, signerOf(t, "mallory-1")); f.Err == nil || f.Err.Code != proto.CodeAuth {
			t.Errorf("mallory's key = %+v, want refused auth", f)
		}
	})
	t.Run("a key the server knows", func(t *testing.T) {
		if f := connectWith(t, addr, pool, signerOf(t, "dana-1")); f.Err != nil || f.Principal != "dana" {
			t.Errorf("dana's key = %+v, want dana welcomed", f)
		}
	})
	t.Run("no SSH", func(t *testing.T) {
		var out strings.Builder
		_ = server.Bridge(t.Context(), d.sock, "dana", "", strings.NewReader(""), &out)
		f, err := proto.NewDecoder(strings.NewReader(out.String())).Decode()
		if err != nil || f.Err == nil || f.Err.Code != proto.CodeUnavailable || !strings.Contains(f.Err.Message, "SSH is off") {
			t.Errorf("the bridge on a default install = %q, %v; want refused unavailable, SSH is off", out.String(), err)
		}
		if _, err := os.Stat(s.keysFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a default install wrote the keys file %s (stat: %v), which sshd would read", s.keysFile, err)
		}
	})
	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v", err)
	}
}

// With SSH turned on beside HTTPS, the daemon writes the keys file and
// serves the bridge as before.
func TestServeHTTPSAndSSH(t *testing.T) {
	t.Parallel()
	cert, key, pool := tlsFiles(t)
	s := newSite(t, "transports: [https, ssh]\n"+httpsBlock(cert, key))
	keys := writeKeys(t, forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	d := runSite(t, s, nil)
	addr := listening(t, d)
	if f := connectWith(t, addr, pool, signerOf(t, "dana-1")); f.Err != nil || f.Principal != "dana" {
		t.Errorf("dana over HTTPS = %+v, want welcomed", f)
	}
	if _, err := os.Stat(s.keysFile); err != nil {
		t.Errorf("the keys file with SSH on: %v, want it written", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := server.Probe(ctx, d.sock, adopted, "test"); err != nil {
		t.Errorf("the daemon's socket with SSH on: %v", err)
	}
}
