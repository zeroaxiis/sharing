package protocol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Pairing primitives shared by both ends of a pairing exchange.
//
// The threat model is a LAN attacker who can reach the peer port and speak the
// protocol. Pairing defends against it with two things and nothing else: a
// human comparing six digits on two screens, and a rate limit that makes
// guessing those digits hopeless. So the derivation below must be identical on
// both machines, byte for byte, or every pairing fails - and it must never
// depend on a value either side merely received, or a man in the middle could
// make both screens agree on a code it chose.
const (
	// CodeDigits is the length of the human-comparable verification code.
	CodeDigits = 6
	// CodeModulus is 10^CodeDigits, the space the code is reduced into.
	CodeModulus = 1000000

	// NonceBytes is the entropy of a pairing nonce before hex encoding.
	NonceBytes = 16
	// NonceHexLen is the wire length of a nonce, 2 characters per byte.
	NonceHexLen = NonceBytes * 2

	// TokenBytes is the entropy of a pairing token before hex encoding.
	TokenBytes = 32
	// TokenHexLen is the wire length of a token, 2 characters per byte.
	TokenHexLen = TokenBytes * 2
)

// Code derives the six-digit verification code both peers display.
//
//	material = sha256( sorted(idA, idB) joined with ":" , ":" , nonce )
//	code     = first 4 bytes big-endian, mod 1000000, zero-padded to 6 digits
//
// The two device ids are sorted lexicographically (Go's byte-wise string
// comparison) before hashing, so Code(a, b, n) == Code(b, a, n) and neither
// side needs to know whether it dialled or was dialled. The code is never
// transmitted: each side derives it from values it already holds, which is what
// stops a relay from choosing what the humans see.
//
// The modulo introduces a negligible bias (2^32 is not a multiple of 10^6);
// that is irrelevant here because the code is a one-shot human check backed by
// a rate limit, not a key.
func Code(idA, idB, nonce string) string {
	first, second := idA, idB
	if first > second {
		first, second = second, first
	}

	sum := sha256.Sum256([]byte(first + ":" + second + ":" + nonce))
	n := binary.BigEndian.Uint32(sum[:4]) % CodeModulus

	return fmt.Sprintf("%0*d", CodeDigits, n)
}

// NewNonce returns NonceBytes of cryptographic randomness, hex-encoded.
// The responder generates one per pairing attempt and sends it in
// PeerPairRequiredMessage; reusing a nonce would let an observer of an earlier
// pairing recognise the code.
func NewNonce() (string, error) {
	buf := make([]byte, NonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ValidNonceFormat reports whether s is exactly NonceHexLen lowercase hex
// characters, the shape NewNonce produces.
func ValidNonceFormat(s string) bool {
	return validLowerHex(s, NonceHexLen)
}

// ValidTokenFormat reports whether s is exactly TokenHexLen lowercase hex
// characters, the shape config.NewToken produces.
//
// This is a shape check only. Never use it, or any other early return, as a
// stand-in for verifying a token: that must go through
// config.TrustStore.VerifyToken so the comparison is constant time.
func ValidTokenFormat(s string) bool {
	return validLowerHex(s, TokenHexLen)
}

// ValidCodeFormat reports whether s is exactly CodeDigits ASCII digits.
func ValidCodeFormat(s string) bool {
	if len(s) != CodeDigits {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	// Reject anything strconv would not accept as a plain unsigned decimal,
	// which also rules out non-ASCII digits that slipped past the byte range.
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return false
	}
	return true
}

func validLowerHex(s string, want int) bool {
	if len(s) != want {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
