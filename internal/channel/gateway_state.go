package channel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"rhizome/internal/config"
	"strings"
	"time"

	"rhizome/internal/localstate"
)

const gatewayMaxPeers = 16
const gatewayMaxSessions = 64
const gatewayMaxFrame = 256 * 1024

type gatewayPeer struct {
	Host     string `json:"host"`
	Endpoint string `json:"endpoint"`
	Pending  bool   `json:"pending,omitempty"`
	Grant    string `json:"grant"`
}
type gatewayInviteRecord struct {
	Created  time.Time `json:"created"`
	Hash     string    `json:"hash"`
	Expires  time.Time `json:"expires"`
	Redeemed string    `json:"redeemed,omitempty"`
}
type gatewayReceipt struct {
	Host  string    `json:"host"`
	Until time.Time `json:"until"`
}
type gatewayState struct {
	Received map[string]gatewayReceipt      `json:"received"`
	Revoked  map[string]time.Time           `json:"revoked"`
	Version  int                            `json:"version"`
	Key      []byte                         `json:"key"`
	Peers    map[string]gatewayPeer         `json:"peers"`
	Invites  map[string]gatewayInviteRecord `json:"invites"`
}
type gatewayInvitation struct {
	Version  int    `json:"version"`
	Mode     string `json:"mode"`
	Endpoint string `json:"endpoint"`
	Host     string `json:"host"`
	ID       string `json:"id"`
	Secret   string `json:"secret"`
}

func gatewayRandom() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func gatewayDigest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func gatewayEndpoint(address, modeName string) error {
	return config.ValidateGatewayEndpoint(address, modeName)
}
func parseGatewayInvitation(code string) (gatewayInvitation, error) {
	var in gatewayInvitation
	if len(code) > 2048 || !strings.HasPrefix(code, "rhz1_") {
		return in, errors.New("invalid or unsupported invitation")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, "rhz1_"))
	if err != nil || json.Unmarshal(b, &in) != nil || in.Version != 1 || !validFingerprint(in.Host) || !validFingerprint(in.Secret) || !validFingerprint(in.ID) {
		return in, errors.New("invalid or unsupported invitation")
	}
	return in, gatewayEndpoint(in.Endpoint, in.Mode)
}
func gatewayWrite(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gateway-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = localstate.PrivateFile(f.Name())
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}
func gatewayRead(path string, out any) error {
	if err := localstate.PrivateFile(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil {
		return err
	}
	if len(b) > 256*1024 {
		return errors.New("gateway state too large")
	}
	return json.Unmarshal(b, out)
}
func loadGatewayState(path string) (gatewayState, error) {
	s := gatewayState{}
	err := gatewayRead(path, &s)
	if errors.Is(err, os.ErrNotExist) {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return s, e
		}
		s = gatewayState{Revoked: map[string]time.Time{}, Received: map[string]gatewayReceipt{}, Version: 1, Key: key.Seed(), Peers: map[string]gatewayPeer{}, Invites: map[string]gatewayInviteRecord{}}
		return s, gatewayWrite(path, s)
	}
	if err != nil {
		return s, fmt.Errorf("read gateway state: %w", err)
	}
	if s.Version != 1 || len(s.Key) != ed25519.SeedSize || s.Peers == nil || s.Invites == nil || s.Revoked == nil || s.Received == nil || len(s.Revoked)+len(s.Received) > 128 || len(s.Peers) > gatewayMaxPeers || len(s.Invites) > 128 {
		return s, errors.New("invalid gateway state")
	}
	for k, p := range s.Peers {
		if k != p.Host || !validFingerprint(k) || !validFingerprint(p.Grant) {
			return s, errors.New("invalid peer state")
		}
	}
	for k, v := range s.Invites {
		if !validFingerprint(k) || !validFingerprint(v.Hash) || v.Expires.IsZero() || v.Created.IsZero() || (v.Redeemed != "" && !validFingerprint(v.Redeemed)) {
			return s, errors.New("invalid invitation state")
		}
	}
	for id, r := range s.Received {
		if !validFingerprint(id) || !validFingerprint(r.Host) || r.Until.IsZero() {
			return s, errors.New("invalid join receipt state")
		}
	}
	for id, until := range s.Revoked {
		if !validFingerprint(id) || until.IsZero() {
			return s, errors.New("invalid revocation state")
		}
	}
	return s, nil
}
func gatewayCertificate(seed []byte) (tls.Certificate, string, error) {
	key := ed25519.NewKeyFromSeed(seed)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Rhizome gateway"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, fingerprint(cert), nil
}

func encodeGatewayInvitation(b []byte) string {
	return "rhz1_" + base64.RawURLEncoding.EncodeToString(b)
}

func gatewayDeliveryGrants(runtime string) map[string]string {
	out := map[string]string{}
	if runtime == "" {
		return out
	}
	var state gatewayState
	if gatewayRead(filepath.Join(runtime, "gateway", "state.json"), &state) != nil || state.Version != 1 {
		return out
	}
	for h, p := range state.Peers {
		if !p.Pending {
			out[h] = p.Grant
		}
	}
	return out
}
