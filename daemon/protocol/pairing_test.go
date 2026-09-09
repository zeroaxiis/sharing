package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"testing"
)

const (
	idA   = "8f3a2d91-4c1b-4a77-9e02-1f6b5c3d0a11"
	idB   = "0a1b2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d"
	nonce = "9ca55654a3f14c0f8b2d7e6a5c4b3a29"
)

// The whole pairing story rests on this: two machines that never exchange the
// code must still show the same six digits. Since only one of them knows which
// side dialled, argument order cannot be allowed to matter.
func TestCodeIsOrderIndependent(t *testing.T) {
	ab := Code(idA, idB, nonce)
	ba := Code(idB, idA, nonce)
	if ab != ba {
		t.Fatalf("Code is order dependent: Code(a,b,n)=%q Code(b,a,n)=%q", ab, ba)
	}
}

func TestCodeVariesWithNonce(t *testing.T) {
	first := Code(idA, idB, nonce)
	second := Code(idA, idB, "0000000000000000000000000000dead")
	if first == second {
		t.Fatalf("different nonces produced the same code: %q", first)
	}
	// And in the other argument order too, so a regression cannot hide behind
	// the sort.
	if Code(idB, idA, nonce) == Code(idB, idA, "0000000000000000000000000000dead") {
		t.Fatal("different nonces produced the same code with swapped ids")
	}
}

func TestCodeVariesWithPeer(t *testing.T) {
	if Code(idA, idB, nonce) == Code(idA, "11111111-2222-4333-8444-555555555555", nonce) {
		t.Fatal("a different peer id produced the same code")
	}
}

func TestCodeFormat(t *testing.T) {
	// Sweep enough nonces to hit a value that needs zero padding.
	sawPadded := false
	for i := 0; i < 4096; i++ {
		got := Code(idA, idB, fmt.Sprintf("%032x", i))
		if len(got) != CodeDigits {
			t.Fatalf("code %q is %d characters, want %d", got, len(got), CodeDigits)
		}
		if !ValidCodeFormat(got) {
			t.Fatalf("code %q is not %d ASCII digits", got, CodeDigits)
		}
		if got[0] == '0' {
			sawPadded = true
		}
	}
	if !sawPadded {
		t.Fatal("never produced a zero-padded code in 4096 tries; padding is untested")
	}
}

// Pin the derivation to the spec so a refactor cannot quietly change the digits
// and break interoperability with an already-shipped peer.
func TestCodeMatchesSpecDerivation(t *testing.T) {
	// idB sorts before idA, so the spec's material is idB:idA:nonce.
	sum := sha256.Sum256([]byte(idB + ":" + idA + ":" + nonce))
	want := fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1000000)

	if got := Code(idA, idB, nonce); got != want {
		t.Fatalf("Code = %q, spec derivation = %q", got, want)
	}
}

func TestCodeSameDeviceID(t *testing.T) {
	// Degenerate but well defined: the sort is a no-op and the code is stable.
	if Code(idA, idA, nonce) != Code(idA, idA, nonce) {
		t.Fatal("Code is not deterministic for identical ids")
	}
}

func TestNewNonce(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		n, err := NewNonce()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidNonceFormat(n) {
			t.Fatalf("nonce %q is not %d lowercase hex characters", n, NonceHexLen)
		}
		if _, dup := seen[n]; dup {
			t.Fatalf("NewNonce repeated a value: %q", n)
		}
		seen[n] = struct{}{}
	}
}

func TestFormatValidators(t *testing.T) {
	if ValidNonceFormat("") || ValidNonceFormat("ABCDEF0123456789abcdef0123456789") {
		t.Fatal("ValidNonceFormat accepted a bad nonce")
	}
	if !ValidTokenFormat("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff") {
		t.Fatal("ValidTokenFormat rejected a well-formed token")
	}
	if ValidTokenFormat("00112233445566778899aabbccddeeff") {
		t.Fatal("ValidTokenFormat accepted a 32-character token")
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", "  1234", "１２３４５６"} {
		if ValidCodeFormat(bad) {
			t.Fatalf("ValidCodeFormat accepted %q", bad)
		}
	}
	if !ValidCodeFormat("000000") || !ValidCodeFormat("482913") {
		t.Fatal("ValidCodeFormat rejected a well-formed code")
	}
}
