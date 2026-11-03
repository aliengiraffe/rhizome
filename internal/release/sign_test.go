package release

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	priv, seed, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSigningKey(seed)
	if err != nil || !parsed.Equal(priv) {
		t.Fatalf("seed round trip: %v", err)
	}
	return priv, priv.Public().(ed25519.PublicKey)
}

func TestSignatureRoundTrip(t *testing.T) {
	priv, pub := testKey(t)
	manifest := []byte("abc  rhizome_1.0.0_linux_amd64.tar.gz\n")
	sig := Sign(manifest, priv)
	if !strings.HasPrefix(string(sig), signatureHeader+" "+KeyID(pub)+" ") || !strings.HasSuffix(string(sig), "\n") {
		t.Fatalf("signature file %q", sig)
	}
	id, err := VerifySignature(manifest, sig, []ed25519.PublicKey{pub})
	if err != nil || id != KeyID(pub) {
		t.Fatalf("verify: %s %v", id, err)
	}
	// Any key in the list may sign; the id selects it.
	_, other := testKey(t)
	if _, err := VerifySignature(manifest, sig, []ed25519.PublicKey{other, pub}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySignatureRejects(t *testing.T) {
	priv, pub := testKey(t)
	otherPriv, otherPub := testKey(t)
	manifest := []byte("abc  rhizome_1.0.0_linux_amd64.tar.gz\n")
	good := Sign(manifest, priv)
	cases := []struct {
		name      string
		manifest  []byte
		signature []byte
		keys      []ed25519.PublicKey
		want      string
	}{
		{"no trusted keys", manifest, good, nil, "untrusted key"},
		{"wrong key", manifest, good, []ed25519.PublicKey{otherPub}, "untrusted key"},
		{"other signer", manifest, Sign(manifest, otherPriv), []ed25519.PublicKey{pub}, "untrusted key"},
		{"tampered manifest", append([]byte("x"), manifest...), good, []ed25519.PublicKey{pub}, "does not verify"},
		{"tampered signature", []byte(strings.Replace(string(good), "AAAA", "AAAB", 1)), good, []ed25519.PublicKey{pub}, ""},
		{"truncated", manifest, good[:len(good)-10], []ed25519.PublicKey{pub}, "malformed signature"},
		{"empty", manifest, nil, []ed25519.PublicKey{pub}, "unrecognized"},
		{"wrong header", manifest, []byte(strings.Replace(string(good), "v1", "v2", 1)), []ed25519.PublicKey{pub}, "unrecognized"},
		{"extra line", manifest, append(good, "more\n"...), []ed25519.PublicKey{pub}, "single line"},
		{"oversize", manifest, append(good, make([]byte, maxSignature)...), []ed25519.PublicKey{pub}, "too large"},
		{"key id substituted", manifest, []byte(strings.Replace(string(good), KeyID(pub), KeyID(otherPub), 1)), []ed25519.PublicKey{pub, otherPub}, "does not verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "tampered signature" {
				// A changed digit in the signature either fails decoding or verification.
				_, err := VerifySignature(tc.manifest, tc.signature, tc.keys)
				if err == nil {
					t.Fatal("accepted")
				}
				return
			}
			_, err := VerifySignature(tc.manifest, tc.signature, tc.keys)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSignedManifestFiles(t *testing.T) {
	priv, pub := testKey(t)
	dir := writeAssets(t, "1.2.3")
	if err := WriteManifest(dir, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedManifest(dir, "1.2.3", []ed25519.PublicKey{pub}); err == nil {
		t.Fatal("missing signature accepted")
	}
	if err := SignManifest(dir, "1.2.3", priv); err != nil {
		t.Fatal(err)
	}
	if id, err := VerifySignedManifest(dir, "1.2.3", []ed25519.PublicKey{pub}); err != nil || id != KeyID(pub) {
		t.Fatal(id, err)
	}
	// The signature file is allowed next to the assets in exact mode.
	if err := VerifyManifest(dir, "1.2.3", true); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedManifest(dir, "1.2.3", nil); err == nil {
		t.Fatal("empty trust list accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName("1.2.3")), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedManifest(dir, "1.2.3", []ed25519.PublicKey{pub}); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestTrustedKeysParse(t *testing.T) {
	if _, err := TrustedKeys(); err != nil {
		t.Fatal(err)
	}
	_, pub := testKey(t)
	text := strings.ToLower(keyHex(pub))
	if _, err := ParsePublicKeys([]string{text, text}); err == nil {
		t.Fatal("duplicate accepted")
	}
	for _, bad := range []string{"", "zz", strings.ToUpper(text), text[:10], text + "00"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if k, err := ParsePublicKey(text); err != nil || !k.Equal(pub) {
		t.Fatal(err)
	}
}

func TestParseManifest(t *testing.T) {
	dir := writeAssets(t, "1.2.3")
	if err := WriteManifest(dir, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ManifestName("1.2.3")))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ParseManifest(data, "1.2.3")
	if err != nil || len(sums) != len(Assets("1.2.3")) {
		t.Fatal(err, sums)
	}
	for _, bad := range []string{
		"", string(data)[1:], string(data) + string(data), strings.Replace(string(data), "linux_amd64", "linux_mips", 1),
		strings.Replace(string(data), "  ", " ", 1), strings.ToUpper(string(data)),
	} {
		if _, err := ParseManifest([]byte(bad), "1.2.3"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ParseManifest(data, "1.2.4"); err == nil {
		t.Error("manifest for another version accepted")
	}
}
