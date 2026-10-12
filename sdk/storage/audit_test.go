package storage

import (
	"bytes"
	"testing"
)

// TestAuditProofVector checks AuditProof against values from Python:
//
//	hashlib.sha256(b"arc-storage-audit-v1" + b"\x00" + nonce + bytes.fromhex("11"*32) + b"a"*4096).hexdigest()
//
// with nonce = bytes(range(32)), and then bytes(range(1, 33)).
func TestAuditProofVector(t *testing.T) {
	id := "1111111111111111111111111111111111111111111111111111111111111111"
	blob := bytes.Repeat([]byte("a"), 4096)
	nonce := make([]byte, NonceSize)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	if got := AuditProof(nonce, id, blob); got != "057bac426e1142de27e26ba9262dcc4a9ae6624d981a2f57ef3e95632804d49a" {
		t.Fatalf("AuditProof = %s", got)
	}
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	if got := AuditProof(nonce, id, blob); got != "dee61c62fcaf3217aab7874259f761c339a662d3aae7369e0f725e445faf5bf9" {
		t.Fatalf("AuditProof of the second nonce = %s", got)
	}
}
