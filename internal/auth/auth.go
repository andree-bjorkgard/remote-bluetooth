// Package auth builds mutual TLS configurations from a shared secret.
//
// Every host that knows the secret derives the same Ed25519 CA key from it.
// Each process creates a short-lived leaf certificate signed by that CA, and
// both sides of a connection require the peer's certificate to chain to it.
// The result is encrypted, mutually authenticated TLS 1.3 without any
// certificate files: knowing the secret is membership.
//
// Because a peer's certificate lets an attacker test guesses of the secret
// offline, the secret must be random; GenerateSecret makes a suitable one.
package auth

import (
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"time"
)

// ServerName is the name every server certificate is issued for and every
// client verifies, independent of the address it dialed.
const ServerName = "remote-bluetooth"

// MinSecretLength is the shortest secret accepted.
const MinSecretLength = 16

// ErrWeakSecret is returned for a missing or too short secret.
var ErrWeakSecret = fmt.Errorf("auth: secret must be at least %d characters (generate one with `remote-bluetooth gen-secret`)", MinSecretLength)

const (
	kdfSalt       = "remote-bluetooth/v2 ca"
	kdfIterations = 200_000
	leafLifetime  = 10 * 365 * 24 * time.Hour
)

// GenerateSecret returns a new random secret.
func GenerateSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails, see crypto/rand
	return base64.RawURLEncoding.EncodeToString(b)
}

// Identity is the TLS material derived from one secret.
type Identity struct {
	pool *x509.CertPool
	leaf tls.Certificate
}

// New derives the CA from secret and issues this process's leaf certificate.
func New(secret string) (*Identity, error) {
	if len(secret) < MinSecretLength {
		return nil, ErrWeakSecret
	}
	seed, err := pbkdf2.Key(sha256.New, secret, []byte(kdfSalt), kdfIterations, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("auth: derive key: %w", err)
	}
	caKey := ed25519.NewKeyFromSeed(seed)

	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "remote-bluetooth CA"},
		NotBefore:             time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		return nil, fmt.Errorf("auth: create CA: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("auth: parse CA: %w", err)
	}

	leafPub, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("auth: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("auth: serial: %w", err)
	}
	now := time.Now()
	leafTmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: ServerName},
		DNSNames:     []string{ServerName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, leafPub, caKey)
	if err != nil {
		return nil, fmt.Errorf("auth: create certificate: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &Identity{
		pool: pool,
		leaf: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
	}, nil
}

// ServerConfig returns a TLS config that only accepts clients with the same
// secret.
func (id *Identity) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.leaf},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    id.pool,
	}
}

// ClientConfig returns a TLS config that only trusts servers with the same
// secret.
func (id *Identity) ClientConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.leaf},
		RootCAs:      id.pool,
		ServerName:   ServerName,
	}
}
