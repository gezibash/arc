// Package direct carries packets between two citizens over one connection of
// their own, once their relay has introduced them.
//
// The carrier is TLS 1.3. Each side makes a certificate that lives for one
// day, and the relay conversation names the exact certificate of the other
// side by its hash. A certificate that does not match that hash ends the
// connection, so no other party can take the place of the peer.
//
// After the handshake, each side proves its ARC identity. The proof covers
// the keys of the TLS channel, so a proof of one connection never counts on
// another.
package direct

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"time"
)

// CredentialLife is how long the certificate of a carrier lives. It is short:
// the relay names it for one conversation.
const CredentialLife = 24 * time.Hour

// Credentials are the certificate of one side, and its hash.
type Credentials struct {
	// Certificate is what TLS presents.
	Certificate tls.Certificate
	// Fingerprint is the SHA-256 of the certificate, which the relay
	// conversation carries to the other side.
	Fingerprint []byte
}

// NewCredentials makes one certificate for one conversation. It holds no
// name, and nothing trusts it but the hash that the peers exchanged.
func NewCredentials() (*Credentials, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	// The certificate starts an hour ago, so a small difference between two
	// clocks does not refuse it.
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "arc-direct"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(CredentialLife),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256(der)
	return &Credentials{
		Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		Fingerprint: digest[:],
	}, nil
}

// tlsConfig builds the TLS settings of one side. The only certificate that
// passes is the one whose hash the relay conversation carried.
func (o *Options) tlsConfig(server bool) *tls.Config {
	expected := o.PeerFingerprint

	config := &tls.Config{
		Certificates: []tls.Certificate{o.Credentials.Certificate},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		// The certificate of the peer is named by its hash, not by a name
		// or an authority, so the check below is the whole of it.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return ErrNoCertificate
			}

			digest := sha256.Sum256(raw[0])
			if string(digest[:]) != string(expected) {
				return ErrCertificateMismatch
			}
			return nil
		},
	}

	if server {
		config.ClientAuth = tls.RequireAnyClientCert
	}
	return config
}
