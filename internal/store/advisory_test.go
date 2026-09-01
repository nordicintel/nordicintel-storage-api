package store

import "testing"

// TestAdvisoryKeyMatchesTheDocumentedByteLayout pins the lock key against
// digests computed independently from the specification: each normalized UTF-8
// key is prefixed with its length as an unsigned 64-bit big-endian integer, the
// provider is concatenated with the dataset, the result is hashed with SHA-256,
// and the first eight digest bytes are read as a signed big-endian int64.
func TestAdvisoryKeyMatchesTheDocumentedByteLayout(t *testing.T) {
	cases := []struct {
		provider string
		dataset  string
		want     int64
	}{
		{"scb", "population", -272432639003633402},
		{"a", "b", 4367745140043581558},
		{"", "", 3983162290893594069},
		{"ångström", "mätning", 398470525250598311},
	}
	for _, tc := range cases {
		if got := advisoryKey(tc.provider, tc.dataset); got != tc.want {
			t.Fatalf("advisoryKey(%q, %q) = %d, want %d", tc.provider, tc.dataset, got, tc.want)
		}
	}
}

// TestAdvisoryKeyIsUnambiguousAcrossTheSeparator proves the length prefixes stop
// "ab"/"c" and "a"/"bc" from hashing to the same lock key.
func TestAdvisoryKeyIsUnambiguousAcrossTheSeparator(t *testing.T) {
	if advisoryKey("ab", "c") == advisoryKey("a", "bc") {
		t.Fatal("identities that differ only in the split point share a lock key")
	}
	if got, want := advisoryKey("ab", "c"), int64(6925784671553650220); got != want {
		t.Fatalf("advisoryKey(ab, c) = %d, want %d", got, want)
	}
	if got, want := advisoryKey("a", "bc"), int64(4589064456534337473); got != want {
		t.Fatalf("advisoryKey(a, bc) = %d, want %d", got, want)
	}
}

func TestAdvisoryKeyIsDeterministic(t *testing.T) {
	first := advisoryKey("scb", "population")
	for range 100 {
		if advisoryKey("scb", "population") != first {
			t.Fatal("advisoryKey is not deterministic")
		}
	}
}
