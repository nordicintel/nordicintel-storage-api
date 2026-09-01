package domain

import (
	"strconv"
	"testing"
	"unicode/utf8"
)

func FuzzNormalizeCodeIsIdempotent(f *testing.F) {
	for _, seed := range []string{
		"", " ", "scb", "SCB", "  ﬁN  ", "Ⅻ", "ẞ", "Å", "İ",
		"　　", "K", "²", "Ｓｃｂ", "population", "㍿",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spelling string) {
		if !utf8.ValidString(spelling) {
			t.Skip("path codes are decoded as UTF-8 before normalization")
		}
		first, err := NormalizeCode(spelling)
		if err != nil {
			return
		}
		if first.Key == "" {
			t.Fatalf("NormalizeCode(%q) returned an empty key without an error", spelling)
		}
		if len(first.Key) > MaxCodeBytes {
			t.Fatalf("NormalizeCode(%q) returned a %d byte key", spelling, len(first.Key))
		}
		second, err := NormalizeCode(first.Key)
		if err != nil {
			t.Fatalf("normalizing the key %q of %q failed: %v", first.Key, spelling, err)
		}
		if second.Key != first.Key {
			t.Fatalf("normalization is not idempotent: %q -> %q -> %q", spelling, first.Key, second.Key)
		}
	})
}

func FuzzParseSparseIndex(f *testing.F) {
	for _, seed := range []string{"", "0", "1", "01", "-1", "+1", "1.0", "a", "999", "1000000",
		"00", " 1", "1 ", "0x1", "9223372036854775808", "0000000000000000001"} {
		f.Add(seed, int64(1000))
	}
	f.Fuzz(func(t *testing.T, key string, count int64) {
		if count < 1 || count > 1_000_000 {
			t.Skip("cell counts outside the schema range never reach the parser")
		}
		index, err := parseSparseIndex(key, count)
		if err != nil {
			return
		}
		if index < 0 || index >= count {
			t.Fatalf("parseSparseIndex(%q, %d) accepted the out-of-range index %d", key, count, index)
		}
		// An accepted key must be the canonical base-10 spelling of its value,
		// so re-encoding it reproduces the key exactly.
		if canonical := strconv.FormatInt(index, 10); canonical != key {
			t.Fatalf("parseSparseIndex accepted the non-canonical key %q for index %d", key, index)
		}
	})
}

func FuzzCoordinateRoundTrip(f *testing.F) {
	f.Add(int64(0), 2, 3, 4)
	f.Add(int64(23), 2, 3, 4)
	f.Add(int64(5), 1, 1, 6)
	f.Add(int64(999999), 100, 100, 100)
	f.Fuzz(func(t *testing.T, index int64, a, b, c int) {
		sizes := []int{a, b, c}
		for _, size := range sizes {
			if size < 1 || size > 1_000_000 {
				t.Skip("dimension sizes outside the schema range never reach the mapper")
			}
		}
		count, err := CheckedProduct(sizes, 1_000_000)
		if err != nil {
			t.Skip("cubes over the cell limit are rejected before mapping")
		}
		strides := Strides(sizes)
		coords, err := Coordinates(index, sizes, strides)
		if err != nil {
			if index >= 0 && index < count {
				t.Fatalf("Coordinates(%d, %v) rejected an index inside the cube: %v", index, sizes, err)
			}
			return
		}
		if index < 0 || index >= count {
			t.Fatalf("Coordinates(%d, %v) accepted an index outside the cube", index, sizes)
		}
		back, err := CellIndex(coords, sizes, strides)
		if err != nil {
			t.Fatalf("CellIndex(%v, %v): %v", coords, sizes, err)
		}
		if back != index {
			t.Fatalf("round trip %d -> %v -> %d for sizes %v", index, coords, back, sizes)
		}
	})
}

func FuzzParseReplacementNeverPanics(f *testing.F) {
	seeds := []string{
		`{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`,
		`{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":{"0":1}}`,
		`{"source_stamp":{"e":1},"id":["a","b"],"dimension":{"a":{"index":{"x":0,"y":1}},"b":{"index":{"p":0}}},"value":[1,null],"text":[null,"t"],"status":"c"}`,
		`{}`, `[]`, `null`, `{"source_stamp":null}`, ``,
		`{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":{"-0":1}}`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		replacement, err := ParseReplacement("provider", "dataset", []byte(body), 1_000_000)
		if err != nil {
			return
		}
		if replacement.CellCount < 1 || replacement.CellCount > 1_000_000 {
			t.Fatalf("accepted body produced cell count %d", replacement.CellCount)
		}
		if replacement.ValuedCount < 0 || replacement.ValuedCount > replacement.CellCount {
			t.Fatalf("valued count %d is outside 0..%d", replacement.ValuedCount, replacement.CellCount)
		}
		if len(replacement.Dimensions) < 1 || len(replacement.Dimensions) > MaxDimensions {
			t.Fatalf("accepted body produced %d dimensions", len(replacement.Dimensions))
		}
		seen := make(map[int64]struct{}, len(replacement.Cells))
		for i, cell := range replacement.Cells {
			if cell.Index < 0 || cell.Index >= replacement.CellCount {
				t.Fatalf("cell index %d is outside 0..%d", cell.Index, replacement.CellCount)
			}
			if _, duplicate := seen[cell.Index]; duplicate {
				t.Fatalf("duplicate internal cell index %d", cell.Index)
			}
			seen[cell.Index] = struct{}{}
			if i > 0 && replacement.Cells[i-1].Index >= cell.Index {
				t.Fatal("cells are not sorted by ascending index")
			}
			if cell.Numeric != nil && cell.Text != nil {
				t.Fatalf("cell %d carries both a numeric and a text value", cell.Index)
			}
			if cell.Numeric == nil && cell.Text == nil && cell.Status == nil {
				t.Fatalf("cell %d is empty and should not have been stored", cell.Index)
			}
		}
		// The stored structure is always sorted by normalized key with
		// contiguous positions, whatever order the payload used.
		for i, dimension := range replacement.Dimensions {
			if dimension.Position != i {
				t.Fatalf("dimension %d has position %d", i, dimension.Position)
			}
			if i > 0 && replacement.Dimensions[i-1].Code.Key >= dimension.Code.Key {
				t.Fatal("dimensions are not sorted by normalized key")
			}
			for j, category := range dimension.Categories {
				if category.Position != j {
					t.Fatalf("category %d of dimension %q has position %d", j, dimension.Code.Key, category.Position)
				}
				if j > 0 && dimension.Categories[j-1].Code.Key >= category.Code.Key {
					t.Fatal("categories are not sorted by normalized key")
				}
			}
		}
	})
}

func FuzzParseSelectionNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`{"id":["a"],"dimension":{"a":{"index":{"x":0}}}}`,
		`{"id":["a","b"],"dimension":{"a":{"index":{"x":0,"y":1}},"b":{"index":{"p":0}}}}`,
		`{}`, `[]`, `{"id":[]}`, ``,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		selection, err := ParseSelection([]byte(body))
		if err != nil {
			return
		}
		if len(selection.Dimensions) < 1 || len(selection.Dimensions) > MaxDimensions {
			t.Fatalf("accepted selector produced %d dimensions", len(selection.Dimensions))
		}
		for _, dimension := range selection.Dimensions {
			if dimension.Code.Key == "" {
				t.Fatal("accepted selector produced an empty dimension key")
			}
			if len(dimension.Categories) < 1 {
				t.Fatalf("dimension %q has no categories", dimension.Code.Key)
			}
			for _, category := range dimension.Categories {
				if category.Key == "" {
					t.Fatalf("dimension %q has an empty category key", dimension.Code.Key)
				}
			}
		}
	})
}
