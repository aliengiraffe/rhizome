package oai

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
)

var errIdentity = errors.New("ID token verification failed; sign in again if the failure persists")

type identity struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	AZP       string          `json:"azp"`
	Nonce     string          `json:"nonce"`
	Expires   int64           `json:"exp"`
	Issued    int64           `json:"iat"`
	NotBefore int64           `json:"nbf"`
	Email     string          `json:"email"`
	Name      string          `json:"name"`
}
type jwk struct {
	Kty string   `json:"kty"`
	Kid string   `json:"kid"`
	Alg string   `json:"alg"`
	Use string   `json:"use"`
	Ops []string `json:"key_ops"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	Crv string   `json:"crv"`
	X   string   `json:"x"`
	Y   string   `json:"y"`
}

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
func (c *Client) verify(ctx context.Context, d discovery, token, clientID, nonce string) (identity, error) {
	var claims identity
	if len(token) > 64<<10 {
		return claims, errIdentity
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errIdentity
	}
	var h struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	head, e := decode(parts[0])
	if e != nil || json.Unmarshal(head, &h) != nil || h.Kid == "" || len(h.Crit) > 0 || !slices.Contains(d.Algorithms, h.Alg) || (h.Alg != "RS256" && h.Alg != "ES256") {
		return claims, errIdentity
	}
	sig, e := decode(parts[2])
	if e != nil {
		return claims, errIdentity
	}
	var keys struct {
		Keys []jwk `json:"keys"`
	}
	if e = c.jsonRequest(ctx, "GET", d.JWKS, "", "", nil, &keys); e != nil {
		return claims, e
	}
	var key *jwk
	for i := range keys.Keys {
		k := &keys.Keys[i]
		if k.Kid == h.Kid {
			if key != nil {
				return claims, errIdentity
			}
			key = k
		}
	}
	if key == nil || (key.Alg != "" && key.Alg != h.Alg) || (key.Use != "" && key.Use != "sig") || (len(key.Ops) > 0 && !slices.Contains(key.Ops, "verify")) {
		return claims, errIdentity
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	verified := false
	if h.Alg == "RS256" && key.Kty == "RSA" {
		nb, en := decode(key.N)
		eb, ee := decode(key.E)
		if en == nil && ee == nil && len(eb) > 0 && len(eb) <= 4 {
			n := new(big.Int).SetBytes(nb)
			ex := new(big.Int).SetBytes(eb).Int64()
			if n.BitLen() >= 2048 && n.BitLen() <= 8192 && ex >= 3 && ex <= 2147483647 && ex%2 == 1 {
				verified = rsa.VerifyPKCS1v15(&rsa.PublicKey{N: n, E: int(ex)}, crypto.SHA256, digest[:], sig) == nil
			}
		}
	}
	if h.Alg == "ES256" && key.Kty == "EC" && key.Crv == "P-256" && len(sig) == 64 {
		xb, xe := decode(key.X)
		yb, ye := decode(key.Y)
		if xe == nil && ye == nil && len(xb) == 32 && len(yb) == 32 {
			x, y := new(big.Int).SetBytes(xb), new(big.Int).SetBytes(yb)
			curve := elliptic.P256()
			if curve.IsOnCurve(x, y) {
				verified = ecdsa.Verify(&ecdsa.PublicKey{Curve: curve, X: x, Y: y}, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
			}
		}
	}
	if !verified {
		return claims, errIdentity
	}
	payload, e := decode(parts[1])
	if e != nil || json.Unmarshal(payload, &claims) != nil {
		return claims, errIdentity
	}
	var audiences []string
	var single string
	if json.Unmarshal(claims.Audience, &single) == nil {
		audiences = []string{single}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return claims, errIdentity
	}
	now := c.now().Unix()
	if claims.Issuer != c.issuer || claims.Subject == "" || len(claims.Subject) > 512 || !slices.Contains(audiences, clientID) || (len(audiences) > 1 && claims.AZP != clientID) || (claims.AZP != "" && claims.AZP != clientID) || claims.Expires <= now || claims.NotBefore > now || claims.Issued > now+60 || (nonce != "" && subtle.ConstantTimeCompare([]byte(nonce), []byte(claims.Nonce)) != 1) {
		return identity{}, errIdentity
	}
	return claims, nil
}
