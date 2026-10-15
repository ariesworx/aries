package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cmdCA makes the spike's internal certificate authority, as an
// air-gapped site's own CA stands: a certificate for each server name,
// two client certificates standing in for smart cards (PIV), and a CRL
// that revokes the second. Keys are 0600.
func cmdCA(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	dir := fs.String("dir", "", "directory to write into")
	names := fs.String("names", "aries.internal,keycloak.internal,proxy.internal", "server names, comma-separated")
	_ = fs.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Aries spike internal CA", Organization: []string{"Example"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(7 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, _ := x509.ParseCertificate(caDER)
	if err := writePair(*dir, "ca", caDER, caKey); err != nil {
		return err
	}
	for name := range strings.SplitSeq(*names, ",") {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		t := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(7 * 24 * time.Hour),
			DNSNames: []string{name, "localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, t, ca, &k.PublicKey, caKey)
		if err != nil {
			return err
		}
		if err := writePair(*dir, name, der, k); err != nil {
			return err
		}
	}
	var revoked []x509.RevocationListEntry
	for _, who := range []string{"alice", "mallory"} {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		t := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: who, Organization: []string{"Example"}},
			EmailAddresses: []string{who + "@example.com"},
			NotBefore:      now.Add(-time.Hour), NotAfter: now.Add(7 * 24 * time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, t, ca, &k.PublicKey, caKey)
		if err != nil {
			return err
		}
		if err := writePair(*dir, "card-"+who, der, k); err != nil {
			return err
		}
		if who == "mallory" {
			revoked = append(revoked, x509.RevocationListEntry{SerialNumber: t.SerialNumber, RevocationTime: now})
		}
	}
	crl, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(1), ThisUpdate: now, NextUpdate: now.Add(24 * time.Hour), RevokedCertificateEntries: revoked,
	}, ca, caKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "ca.crl"), pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crl}), 0o644); err != nil {
		return err
	}
	if err := sshKey(*dir); err != nil {
		return err
	}
	fmt.Printf("ca: an internal CA, certificates for %s, two cards (alice; mallory, revoked in ca.crl), and an SSH key whose line forces cat\n", *names)
	return nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func writePair(dir, name string, der []byte, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600)
}

// pool reads a CA file into a pool.
func pool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no certificate", path)
	}
	return p, nil
}

// clientTLS is the client's TLS: the internal CA only, TLS 1.3, and a
// card's certificate if one is named.
func clientTLS(caFile, cardPrefix string) (*tls.Config, error) {
	p, err := pool(caFile)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{RootCAs: p, MinVersion: tls.VersionTLS13}
	if cardPrefix != "" {
		c, err := tls.LoadX509KeyPair(cardPrefix+".pem", cardPrefix+".key")
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{c}
	}
	return cfg, nil
}
