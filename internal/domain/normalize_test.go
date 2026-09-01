package domain

import (
	"strings"
	"testing"
)

func TestNormalizeCodeAppliesTheDocumentedOrder(t *testing.T) {
	cases := []struct {
		name     string
		spelling string
		key      string
	}{
		{"already normalized", "scb", "scb"},
		{"case folded", "SCB", "scb"},
		{"surrounding whitespace trimmed", "  SCB\t\n", "scb"},
		{"ideographic space trimmed", "\u3000SCB\u3000", "scb"},
		{"nfkc compatibility ligature", "\uFB01n", "fin"},
		{"nfkc roman numeral", "\u216B", "xii"},
		{"nfkc superscript", "population\u00B2", "population2"},
		{"full case folding expands sharp s", "\u1E9E", "ss"},
		{"canonical composition", "A\u030Angstr\u00F6m", "\u00E5ngstr\u00F6m"},
		{"kelvin sign folds to latin k", "\u212A", "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := NormalizeCode(tc.spelling)
			if err != nil {
				t.Fatalf("NormalizeCode(%q) returned %v", tc.spelling, err)
			}
			if code.Key != tc.key {
				t.Fatalf("key = %q, want %q", code.Key, tc.key)
			}
			if code.Spelling != tc.spelling {
				t.Fatalf("spelling = %q, want the submitted spelling %q", code.Spelling, tc.spelling)
			}
		})
	}
}

func TestNormalizeCodeIsIdempotent(t *testing.T) {
	for _, spelling := range []string{"SCB", "  \uFB01N  ", "\u216B", "\u1E9E", "A\u030A", "\u0130"} {
		first, err := NormalizeCode(spelling)
		if err != nil {
			t.Fatalf("NormalizeCode(%q): %v", spelling, err)
		}
		second, err := NormalizeCode(first.Key)
		if err != nil {
			t.Fatalf("NormalizeCode(%q): %v", first.Key, err)
		}
		if second.Key != first.Key {
			t.Fatalf("normalization is not idempotent: %q -> %q -> %q", spelling, first.Key, second.Key)
		}
	}
}

func TestNormalizeCodeDetectsCollisions(t *testing.T) {
	groups := [][]string{
		{"SCB", "scb", " Scb ", "\uFF33\uFF43\uFF42"},
		{"\u1E9E", "\u00DF", "ss", "SS"},
		{"\u216B", "XII", "xii"},
	}
	for _, group := range groups {
		var key string
		for i, spelling := range group {
			code, err := NormalizeCode(spelling)
			if err != nil {
				t.Fatalf("NormalizeCode(%q): %v", spelling, err)
			}
			if i == 0 {
				key = code.Key
				continue
			}
			if code.Key != key {
				t.Fatalf("%q normalized to %q, want the colliding key %q", spelling, code.Key, key)
			}
		}
	}
}

func TestNormalizeCodeRejectsInvalidCodes(t *testing.T) {
	cases := []struct {
		name     string
		spelling string
	}{
		{"empty", ""},
		{"only whitespace", "   \t\n"},
		{"only ideographic space", "\u3000"},
		{"submitted spelling too long", strings.Repeat("a", MaxCodeBytes+1)},
		{"multibyte spelling too long", strings.Repeat("\u00E5", MaxCodeBytes/2+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeCode(tc.spelling); err == nil {
				t.Fatalf("NormalizeCode(%q) accepted an invalid code", tc.spelling)
			}
		})
	}
}

func TestNormalizeCodeRejectsNormalizedCodesOverTheByteLimit(t *testing.T) {
	// U+337F (SQUARE CORPORATION) is three submitted bytes and normalizes to
	// four CJK ideographs, so 85 of them stay inside the submitted limit while
	// expanding far past the normalized limit.
	spelling := strings.Repeat("㍿", 85)
	if len(spelling) > MaxCodeBytes {
		t.Fatalf("test fixture spelling is %d bytes, which the submitted limit already rejects", len(spelling))
	}
	if _, err := NormalizeCode(spelling); err == nil {
		t.Fatal("normalized code over the byte limit was accepted")
	}
}
