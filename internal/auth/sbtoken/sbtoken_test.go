package sbtoken

import "testing"

func TestMintVerify(t *testing.T) {
	tok, hash := Mint()
	if len(tok) != 64 || len(hash) != 64 { // 32 bytes hex each
		t.Fatalf("unexpected lengths: token=%d hash=%d", len(tok), len(hash))
	}
	if !Verify(tok, hash) {
		t.Fatal("minted token failed to verify against its own hash")
	}
	tok2, hash2 := Mint()
	if tok == tok2 || hash == hash2 {
		t.Fatal("Mint produced a repeat token/hash")
	}
	if Verify(tok, hash2) {
		t.Fatal("a token verified against the WRONG hash")
	}
}

func TestVerifyRejectsEmpty(t *testing.T) {
	_, hash := Mint()
	if Verify("", hash) {
		t.Fatal("empty token authenticated")
	}
	if Verify("something", "") {
		t.Fatal("empty expected hash authenticated")
	}
	if Verify("", "") {
		t.Fatal("empty/empty authenticated")
	}
}

func TestHashHexStable(t *testing.T) {
	// Known SHA-256 of "abc", so an accidental algorithm change fails here.
	if got := HashHex("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("HashHex(abc) = %s", got)
	}
}
