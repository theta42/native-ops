package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"testing"
)

// AWS-documented SigV4 test vector: signing key derivation.
// https://docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html
func TestSigningKeyVector(t *testing.T) {
	key := signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	got := hex.EncodeToString(key)
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Fatalf("signing key = %s, want %s", got, want)
	}
}

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"a b/c", false, "a%20b/c"},
		{"a b/c", true, "a%20b%2Fc"},
		{"azAZ09-_.~", true, "azAZ09-_.~"},
		{"k=v&x", true, "k%3Dv%26x"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("uriEncode(%q,%v) = %q, want %q", c.in, c.encodeSlash, got, c.want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	q := url.Values{"b": {"2"}, "a": {"1"}, "c": {"z", "a"}}
	if got, want := canonicalQuery(q), "a=1&b=2&c=a&c=z"; got != want {
		t.Fatalf("canonicalQuery = %q, want %q", got, want)
	}
}

func TestSHA256Hex(t *testing.T) {
	sum := sha256.Sum256([]byte("hello-backup"))
	if got := sha256Hex([]byte("hello-backup")); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256Hex mismatch: %s", got)
	}
	if got := sha256Hex([]byte("")); got != emptySHA256 {
		t.Fatalf("empty hash = %s, want %s", got, emptySHA256)
	}
}
