package domain

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// MaxNormalizationRounds bounds the search for a normalization fixed point.
// One round is not always a fixed point: compatibility decomposition can
// introduce surrounding whitespace the leading trim has already passed
// (U+00B8 CEDILLA decomposes to a space plus U+0327 COMBINING CEDILLA), and
// case folding can produce characters that decompose further. Repeating the
// round guarantees that normalizing a normalized key returns the key itself, so
// one logical code can never be stored under two identities. A code that has
// not converged within this many rounds is invalid, which keeps normalization
// terminating and deterministic for every input.
const MaxNormalizationRounds = 8

var unicodeFold = cases.Fold()

// NormalizeCode derives the identity key for a submitted code spelling while
// preserving the spelling itself for responses.
func NormalizeCode(spelling string) (Code, error) {
	if len(spelling) > MaxCodeBytes {
		return Code{}, fmt.Errorf("code spelling exceeds %d UTF-8 bytes", MaxCodeBytes)
	}
	key := spelling
	converged := false
	for range MaxNormalizationRounds {
		next := normalizationRound(key)
		if next == key {
			converged = true
			break
		}
		key = next
	}
	if !converged {
		return Code{}, fmt.Errorf("code does not converge within %d normalization rounds", MaxNormalizationRounds)
	}
	if key == "" {
		return Code{}, fmt.Errorf("code is empty after normalization")
	}
	if len(key) > MaxCodeBytes {
		return Code{}, fmt.Errorf("normalized code exceeds %d UTF-8 bytes", MaxCodeBytes)
	}
	return Code{Spelling: spelling, Key: key}, nil
}

func normalizationRound(value string) string {
	value = strings.TrimFunc(value, unicode.IsSpace)
	value = norm.NFKC.String(value)
	value = unicodeFold.String(value)
	value = norm.NFKC.String(value)
	return strings.TrimFunc(value, unicode.IsSpace)
}
