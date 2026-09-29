// Package nodetls gives a node a self-signed TLS identity that the control
// plane pins by SPKI hash. The pin arrives over the node's authenticated HTTPS
// heartbeat, which is why RequireSecureHop refuses a plaintext ORCHESTRATOR_URL.
package nodetls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// RequireSecureHopEnv is the single switch, read on both tiers, that turns
// every orchestrator/node protection from "prefer" into "refuse".
const RequireSecureHopEnv = "PILLDROPS_REQUIRE_SECURE_HOP"

// RequireSecureHop reports whether enforcement is on. Default off so a control
// plane that upgrades first does not refuse every node that has not.
func RequireSecureHop() bool {
	switch os.Getenv(RequireSecureHopEnv) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// certLifetime is long on purpose: the pin authorises the certificate, so
// expiry does no work and a short lifetime is only a scheduled outage.
const certLifetime = 10 * 365 * 24 * time.Hour

// Identity is a node's TLS keypair and the pin the control plane stores for it.
type Identity struct {
	Cert tls.Certificate
	// SPKI is base64(SHA-256(DER SubjectPublicKeyInfo)) of the leaf's public key.
	SPKI string
}

// SPKI computes the pin for a parsed certificate.
func SPKI(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// LoadOrCreate returns the node's TLS identity from <dir>/node-tls.{crt,key},
// generating one on first call. Persisted because a new pin on every boot
// would be refused by the control plane as a takeover.
func LoadOrCreate(dir string) (*Identity, error) {
	certPath := filepath.Join(dir, "node-tls.crt")
	keyPath := filepath.Join(dir, "node-tls.key")

	if id, err := load(certPath, keyPath); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Never regenerate over an unreadable pair; that changes the pin under
		// a control plane holding the old one.
		return nil, fmt.Errorf("node tls: existing keypair in %s is unusable: %w", dir, err)
	}

	certPEM, keyPEM, err := generate()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("node tls: %w", err)
	}
	// Key first so a racing reader never finds a certificate without its key.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("node tls: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("node tls: %w", err)
	}
	return load(certPath, keyPath)
}

func load(certPath, keyPath string) (*Identity, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	pair.Leaf = leaf
	return &Identity{Cert: pair, SPKI: SPKI(leaf)}, nil
}

// generate mints a self-signed P-256 certificate. The SANs are cosmetic; the
// pin is the authorisation.
func generate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("node tls: keygen: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("node tls: serial: %w", err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "sandboxd-node"
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host, Organization: []string{"sandboxd node"}},
		NotBefore:             now.Add(-time.Hour), // clock skew
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("node tls: sign: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("node tls: marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// ServerConfig is the TLS configuration a node serves with.
func (i *Identity) ServerConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{i.Cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// ErrPinMismatch is returned when the node presents a key the control plane has no pin for.
var ErrPinMismatch = errors.New("node tls pin mismatch")

// PinnedClientConfig builds the client side of the pinned hop. The certificate
// is self-signed and dialled by IP, so InsecureSkipVerify is set and the pin
// check replaces chain and name verification. An empty pin is refused.
func PinnedClientConfig(spki string) (*tls.Config, error) {
	if spki == "" {
		return nil, errors.New("node tls: refusing to build a pinned client with an empty pin")
	}
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // G402: the pin below replaces chain+name verification
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return VerifyPin(spki, rawCerts)
		},
	}, nil
}

// VerifyPin checks a presented chain's leaf against an expected pin.
func VerifyPin(spki string, rawCerts [][]byte) error {
	if spki == "" {
		return errors.New("node tls: no pin to verify against")
	}
	if len(rawCerts) == 0 {
		return fmt.Errorf("%w: node presented no certificate", ErrPinMismatch)
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("%w: leaf unparseable: %w", ErrPinMismatch, err)
	}
	if got := SPKI(leaf); got != spki {
		return fmt.Errorf("%w: node presented %s, control plane pinned %s", ErrPinMismatch, got, spki)
	}
	return nil
}
