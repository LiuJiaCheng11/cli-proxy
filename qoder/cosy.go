package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Qoder's requests are signed rather than simply bearer-authenticated: a random
// AES key is RSA-wrapped into cosyKey, the caller identity is AES-encrypted into
// info, and every request carries md5(payload.cosyKey.date.body.path).

const (
	appCode = "cosy"
	// base64("war, war never changes") — the shared secret behind the auth
	// center's request signature.
	appSecret    = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw=="
	cosyVersion  = "0.1.43"
	clientType   = "5"
	loginVersion = "v2"
	upstreamUA   = "Go-http-client/2.0"
)

// serverPubKeyPEM is the RSA public key the temp AES key is wrapped with.
const serverPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// -----------------------------------------------------------------------------
// Custom base64 variant
// -----------------------------------------------------------------------------

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var stdToCustom [256]byte

func init() {
	for i := 0; i < 256; i++ {
		stdToCustom[i] = byte(i)
	}
	for i := 0; i < 64; i++ {
		stdToCustom[stdAlphabet[i]] = customAlphabet[i]
	}
	stdToCustom['='] = customPad
}

// cosyEncode is the wire encoding for request bodies: standard base64, then a
// fixed three-way block rotation, then an alphabet substitution.
func cosyEncode(data []byte) string {
	std := base64.StdEncoding.EncodeToString(data)
	n := len(std)
	a := n / 3
	rotated := std[n-a:] + std[a:n-a] + std[:a]
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		sb.WriteByte(stdToCustom[rotated[i]])
	}
	return sb.String()
}

// -----------------------------------------------------------------------------
// Session construction
// -----------------------------------------------------------------------------

// identity is the caller the session is bound to — the fields Qoder puts inside
// the AES-encrypted info blob.
type identity struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOauthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// session is the reusable signing material derived from a credential.
type session struct {
	tempKey      []byte
	cosyKey      string // base64(RSA(tempKey))
	info         string // base64(AES(identity, tempKey))
	machineID    string
	machineToken string
	machineType  string
	ident        identity
}

// newSession derives signing material for an identity. The RSA wrap and the AES
// encryption both happen once per credential, not per request.
func newSession(ident identity, machineID, machineToken, machineType string) (*session, error) {
	tempKey := []byte(strings.ReplaceAll(newUUID(), "-", ""))[:16]
	cosyKey, err := rsaWrap(tempKey)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(ident)
	if err != nil {
		return nil, err
	}
	info, err := aesEncrypt(payload, tempKey)
	if err != nil {
		return nil, err
	}
	return &session{
		tempKey:      tempKey,
		cosyKey:      cosyKey,
		info:         info,
		machineID:    machineID,
		machineToken: machineToken,
		machineType:  machineType,
		ident:        ident,
	}, nil
}

func rsaWrap(data []byte) (string, error) {
	block, _ := pem.Decode([]byte(serverPubKeyPEM))
	if block == nil {
		return "", fmt.Errorf("server public key: no PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("server public key: not RSA")
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, data)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// aesEncrypt uses AES-128-CBC with the key doubling as the IV, which is what
// Qoder's Java-style client does.
func aesEncrypt(plaintext, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// -----------------------------------------------------------------------------
// Request signing
// -----------------------------------------------------------------------------

// bearerPayload is the JSON object base64'd into the bearer. Key order matters
// only in that the upstream sorted them; the struct order reproduces it.
type bearerPayload struct {
	CosyVersion string `json:"cosyVersion"`
	IdeVersion  string `json:"ideVersion"`
	Info        string `json:"info"`
	RequestID   string `json:"requestId"`
	Version     string `json:"version"`
}

// signRequest builds the COSY bearer for one call. body is the encoded request
// body and pathSig is the request path with the /algo prefix removed.
func (s *session) signRequest(body, pathSig string, now time.Time) (string, string, error) {
	payload, err := json.Marshal(bearerPayload{
		CosyVersion: cosyVersion,
		IdeVersion:  "",
		Info:        s.info,
		RequestID:   newUUID(),
		Version:     "v1",
	})
	if err != nil {
		return "", "", err
	}
	payloadB64 := base64.StdEncoding.EncodeToString(payload)
	date := strconv.FormatInt(now.Unix(), 10)
	sig := md5Hex(payloadB64 + "\n" + s.cosyKey + "\n" + date + "\n" + body + "\n" + pathSig)
	return "Bearer COSY." + payloadB64 + "." + sig, date, nil
}

// -----------------------------------------------------------------------------
// Auth center signing
// -----------------------------------------------------------------------------

// centerSign is the static signature the auth center expects:
// md5("cosy" & secret & RFC1123 date).
func centerSign(date string) string {
	return md5Hex(appCode + "&" + appSecret + "&" + date)
}

func rfc1123Now() string {
	return time.Now().UTC().Format(http.TimeFormat)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------
// Identifiers
// -----------------------------------------------------------------------------

// newUUID returns a random v4 UUID. Machine ids are generated once per
// credential and persisted so the upstream sees a stable device.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is fatal for signing; a zero UUID would produce a
		// request the upstream rejects anyway.
		panic("qoder: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newMachineToken mirrors the shape the Qoder CLI sends: 50 chars of UUID text,
// base64url encoded.
func newMachineToken() string {
	return base64.RawURLEncoding.EncodeToString([]byte((newUUID() + newUUID())[:50]))
}

// newMachineType is an 18-char hex-ish identifier, matching the CLI.
func newMachineType() string {
	return strings.ReplaceAll(newUUID(), "-", "")[:18]
}
