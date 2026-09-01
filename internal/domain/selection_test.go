package domain

import (
	"errors"
	"testing"
)

// storedTwoByTwo is the stored form of the contract example: dimensions and
// categories in normalized-key order, so the internal index is sex*2 + year.
func storedTwoByTwo() []Dimension {
	return []Dimension{
		{
			Code:     Code{Spelling: "sex", Key: "sex"},
			Position: 0,
			Categories: []Category{
				{Code: Code{Spelling: "F", Key: "f"}, Position: 0},
				{Code: Code{Spelling: "M", Key: "m"}, Position: 1},
			},
		},
		{
			Code:     Code{Spelling: "year", Key: "year"},
			Position: 1,
			Categories: []Category{
				{Code: Code{Spelling: "2024", Key: "2024"}, Position: 0},
				{Code: Code{Spelling: "2025", Key: "2025"}, Position: 1},
			},
		},
	}
}

func selectionOf(pairs ...[]string) Selection {
	selection := Selection{}
	for _, entry := range pairs {
		dimensionCode, err := NormalizeCode(entry[0])
		if err != nil {
			panic(err)
		}
		dimension := SelectionDimension{Code: dimensionCode}
		for _, category := range entry[1:] {
			code, err := NormalizeCode(category)
			if err != nil {
				panic(err)
			}
			dimension.Categories = append(dimension.Categories, code)
		}
		selection.Dimensions = append(selection.Dimensions, dimension)
	}
	return selection
}

func TestResolveSelectionEnumeratesInRequestedOrder(t *testing.T) {
	// Requested order reverses the stored dimension order and the stored sex
	// order, so output index 0 is (2025, M) and index 1 is (2025, F).
	selection := selectionOf([]string{"year", "2025"}, []string{"sex", "M", "F"})
	dimensions, indices, err := ResolveSelection(selection, storedTwoByTwo(), maxCells)
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	if len(dimensions) != 2 || dimensions[0].Code.Key != "year" || dimensions[1].Code.Key != "sex" {
		t.Fatalf("output dimensions = %+v, want year then sex", dimensions)
	}
	if dimensions[0].Position != 0 || dimensions[1].Position != 1 {
		t.Fatalf("output positions = %d, %d; want 0, 1", dimensions[0].Position, dimensions[1].Position)
	}
	sex := dimensions[1].Categories
	if sex[0].Code.Spelling != "M" || sex[0].Position != 0 || sex[1].Code.Spelling != "F" || sex[1].Position != 1 {
		t.Fatalf("sex categories = %+v, want the requested M then F order", sex)
	}
	// (2025, M) is sex position 1 and year position 1 -> 1*2 + 1 = 3.
	// (2025, F) is sex position 0 and year position 1 -> 0*2 + 1 = 1.
	want := []int64{3, 1}
	if len(indices) != len(want) {
		t.Fatalf("indices = %v, want %v", indices, want)
	}
	for i := range want {
		if indices[i] != want[i] {
			t.Fatalf("indices = %v, want %v", indices, want)
		}
	}
}

func TestResolveSelectionReturnsStoredSpellings(t *testing.T) {
	selection := selectionOf([]string{"SEX", "m"}, []string{"YEAR", "2024"})
	dimensions, indices, err := ResolveSelection(selection, storedTwoByTwo(), maxCells)
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	if dimensions[0].Code.Spelling != "sex" || dimensions[1].Code.Spelling != "year" {
		t.Fatalf("dimension spellings = %q, %q; want the stored spellings",
			dimensions[0].Code.Spelling, dimensions[1].Code.Spelling)
	}
	if dimensions[0].Categories[0].Code.Spelling != "M" {
		t.Fatalf("category spelling = %q, want the stored spelling M", dimensions[0].Categories[0].Code.Spelling)
	}
	if len(indices) != 1 || indices[0] != 2 {
		t.Fatalf("indices = %v, want [2]", indices)
	}
}

func TestResolveSelectionCoversTheWholeCubeInStoredOrder(t *testing.T) {
	selection := selectionOf([]string{"sex", "F", "M"}, []string{"year", "2024", "2025"})
	_, indices, err := ResolveSelection(selection, storedTwoByTwo(), maxCells)
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	want := []int64{0, 1, 2, 3}
	if len(indices) != len(want) {
		t.Fatalf("indices = %v, want %v", indices, want)
	}
	for i := range want {
		if indices[i] != want[i] {
			t.Fatalf("indices = %v, want %v", indices, want)
		}
	}
}

func TestResolveSelectionRejectsIncompleteOrUnknownSelections(t *testing.T) {
	cases := []struct {
		name      string
		selection Selection
	}{
		{"missing a dimension", selectionOf([]string{"sex", "F"})},
		{"extra dimension", selectionOf([]string{"sex", "F"}, []string{"year", "2024"}, []string{"age", "0"})},
		{"duplicate dimension", selectionOf([]string{"sex", "F"}, []string{"sex", "M"})},
		{"unknown dimension", selectionOf([]string{"sex", "F"}, []string{"region", "x"})},
		{"unknown category", selectionOf([]string{"sex", "F"}, []string{"year", "2026"})},
		{"duplicate category", selectionOf([]string{"sex", "F", "f"}, []string{"year", "2024"})},
		{"no categories", selectionOf([]string{"sex"}, []string{"year", "2024"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ResolveSelection(tc.selection, storedTwoByTwo(), maxCells); err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
	}
}

func TestResolveSelectionRejectsResultsOverTheCellLimit(t *testing.T) {
	selection := selectionOf([]string{"sex", "F", "M"}, []string{"year", "2024", "2025"})
	_, _, err := ResolveSelection(selection, storedTwoByTwo(), 3)
	var limit CellLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("error = %v, want a CellLimitError", err)
	}
}

func TestResolveSelectionHandlesRepeatedDimensionPermutations(t *testing.T) {
	// A three-dimensional cube proves the enumeration is not accidentally
	// correct only for two dimensions.
	stored := []Dimension{
		{Code: Code{Spelling: "a", Key: "a"}, Position: 0, Categories: []Category{
			{Code: Code{Spelling: "a0", Key: "a0"}, Position: 0},
			{Code: Code{Spelling: "a1", Key: "a1"}, Position: 1},
		}},
		{Code: Code{Spelling: "b", Key: "b"}, Position: 1, Categories: []Category{
			{Code: Code{Spelling: "b0", Key: "b0"}, Position: 0},
			{Code: Code{Spelling: "b1", Key: "b1"}, Position: 1},
			{Code: Code{Spelling: "b2", Key: "b2"}, Position: 2},
		}},
		{Code: Code{Spelling: "c", Key: "c"}, Position: 2, Categories: []Category{
			{Code: Code{Spelling: "c0", Key: "c0"}, Position: 0},
			{Code: Code{Spelling: "c1", Key: "c1"}, Position: 1},
		}},
	}
	// Stored strides are [6, 2, 1]. Request c, a, b in that order.
	selection := selectionOf(
		[]string{"c", "c1", "c0"},
		[]string{"a", "a1"},
		[]string{"b", "b2", "b0", "b1"},
	)
	_, indices, err := ResolveSelection(selection, stored, maxCells)
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	requestedC := []int{1, 0}
	requestedA := []int{1}
	requestedB := []int{2, 0, 1}
	if len(indices) != len(requestedC)*len(requestedA)*len(requestedB) {
		t.Fatalf("got %d indices, want 6", len(indices))
	}
	position := 0
	for _, c := range requestedC {
		for _, a := range requestedA {
			for _, b := range requestedB {
				want := int64(a*6 + b*2 + c)
				if indices[position] != want {
					t.Fatalf("output index %d = %d, want %d", position, indices[position], want)
				}
				position++
			}
		}
	}
}
