package domain

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var unicodeFold = cases.Fold()

func NormalizeCode(spelling string) (Code, error) {
	if len([]byte(spelling)) > MaxCodeBytes {
		return Code{}, fmt.Errorf("code spelling exceeds %d UTF-8 bytes", MaxCodeBytes)
	}
	trimmed := strings.TrimFunc(spelling, unicode.IsSpace)
	key := norm.NFKC.String(trimmed)
	key = unicodeFold.String(key)
	key = norm.NFKC.String(key)
	if key == "" {
		return Code{}, fmt.Errorf("code is empty after normalization")
	}
	if len([]byte(key)) > MaxCodeBytes {
		return Code{}, fmt.Errorf("normalized code exceeds %d UTF-8 bytes", MaxCodeBytes)
	}
	return Code{Spelling: spelling, Key: key}, nil
}
