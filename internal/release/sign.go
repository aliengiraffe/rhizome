package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// signatureHeader starts every signature file; the format is one line:
	// "<header> <key id> <base64 Ed25519 signature>".
	signatureHeader = "rhizome-release-signature v1"
	// signatureDomain separates manifest signatures from any other message
	// a release key might sign.
	signatureDomain = "rhizome release manifest v1\n"
	// maxSignature bounds a signature file read from disk or the network.
	maxSignature = 4 << 10
)

// SignatureName returns the detached signature published next to the manifest.
func SignatureName(assetVersion string) string {
	return ManifestName(assetVersion) + ".sig"
}

// KeyID identifies a public key by the first eight bytes of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ParsePublicKey decodes a lowercase hex Ed25519 public key.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	text = strings.TrimSpace(text)
	if strings.ToLower(text) != text {
		return nil, errors.New("public key must be lowercase hex")
	}
	b, err := hex.DecodeString(text)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d hex-encoded bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// TrustedKeys returns the public keys compiled into this build.
func TrustedKeys() ([]ed25519.PublicKey, error) {
	return ParsePublicKeys(trustedKeys)
}

// ParsePublicKeys decodes every key, rejecting duplicates.
func ParsePublicKeys(texts []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(texts))
	seen := map[string]bool{}
	for _, t := range texts {
		k, err := ParsePublicKey(t)
		if err != nil {
			return nil, fmt.Errorf("trusted key %q: %w", t, err)
		}
		if seen[KeyID(k)] {
			return nil, fmt.Errorf("trusted key %s listed twice", KeyID(k))
		}
		seen[KeyID(k)] = true
		keys = append(keys, k)
	}
	return keys, nil
}

// GenerateSigningKey creates a new Ed25519 key pair and returns the private
// key together with the base64 seed to store as the signing secret.
func GenerateSigningKey() (ed25519.PrivateKey, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return priv, base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

// ParseSigningKey decodes the base64 32-byte seed held by the signing secret.
func ParseSigningKey(seedBase64 string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seedBase64))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key must be the base64 of a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Sign returns the signature file content for manifest.
func Sign(manifest []byte, key ed25519.PrivateKey) []byte {
	sig := ed25519.Sign(key, append([]byte(signatureDomain), manifest...))
	return []byte(signatureHeader + " " + KeyID(key.Public().(ed25519.PublicKey)) + " " + base64.StdEncoding.EncodeToString(sig) + "\n")
}

// VerifySignature checks a signature file against manifest using the trusted
// keys and returns the id of the key that verified it. Every malformed or
// unknown input is an error: an empty key list accepts nothing.
func VerifySignature(manifest, signature []byte, keys []ed25519.PublicKey) (string, error) {
	if len(signature) > maxSignature {
		return "", errors.New("signature file too large")
	}
	line := strings.TrimSuffix(string(signature), "\n")
	if strings.ContainsAny(line, "\r\n") {
		return "", errors.New("signature file must be a single line")
	}
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0]+" "+fields[1] != signatureHeader {
		return "", errors.New("unrecognized signature format")
	}
	keyID := fields[2]
	sig, err := base64.StdEncoding.DecodeString(fields[3])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", errors.New("malformed signature")
	}
	for _, k := range keys {
		if KeyID(k) != keyID {
			continue
		}
		if ed25519.Verify(k, append([]byte(signatureDomain), manifest...), sig) {
			return keyID, nil
		}
		return "", fmt.Errorf("signature by key %s does not verify", keyID)
	}
	return "", fmt.Errorf("signature by untrusted key %s", keyID)
}

// SignManifest writes the detached signature for the manifest in dir.
func SignManifest(dir, assetVersion string, key ed25519.PrivateKey) error {
	manifest, err := readBounded(filepath.Join(dir, ManifestName(assetVersion)), maxManifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, SignatureName(assetVersion)), Sign(manifest, key), 0o644)
}

// VerifySignedManifest verifies the signature file next to the manifest in
// dir and returns the signing key id.
func VerifySignedManifest(dir, assetVersion string, keys []ed25519.PublicKey) (string, error) {
	manifest, err := readBounded(filepath.Join(dir, ManifestName(assetVersion)), maxManifest)
	if err != nil {
		return "", err
	}
	sig, err := readBounded(filepath.Join(dir, SignatureName(assetVersion)), maxSignature)
	if err != nil {
		return "", err
	}
	return VerifySignature(manifest, sig, keys)
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

// ParseManifest parses "<sha256>  <name>" lines and requires that they name
// exactly the expected assets once each. The result maps asset name to digest.
func ParseManifest(data []byte, assetVersion string) (map[string]string, error) {
	if len(data) > maxManifest {
		return nil, errors.New("checksum manifest too large")
	}
	want := Assets(assetVersion)
	seen := map[string]string{}
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := string(raw)
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != sha256.Size*2 || strings.ToLower(sum) != sum {
			return nil, fmt.Errorf("malformed manifest line %q", line)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("malformed digest for %s", name)
		}
		if !contains(want, name) {
			return nil, fmt.Errorf("manifest lists unexpected asset %q", name)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("manifest lists %s twice", name)
		}
		seen[name] = sum
	}
	for _, name := range want {
		if _, ok := seen[name]; !ok {
			return nil, fmt.Errorf("manifest is missing %s", name)
		}
	}
	return seen, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// keyHex renders a public key as the lowercase hex used in trustedKeys.
func keyHex(pub ed25519.PublicKey) string { return hex.EncodeToString(pub) }
