package domain

import (
	"errors"
	"math"
	"testing"
)

func TestCheckedProduct(t *testing.T) {
	cases := []struct {
		name  string
		sizes []int
		limit int64
		want  int64
		err   string
	}{
		{name: "single dimension", sizes: []int{7}, limit: 1_000_000, want: 7},
		{name: "three dimensions", sizes: []int{2, 3, 4}, limit: 1_000_000, want: 24},
		{name: "exactly at the limit", sizes: []int{1000, 1000}, limit: 1_000_000, want: 1_000_000},
		{name: "no dimensions", sizes: nil, limit: 1_000_000, err: "at least one dimension is required"},
		{name: "empty dimension", sizes: []int{2, 0}, limit: 1_000_000, err: "every dimension requires at least one category"},
		{name: "negative dimension", sizes: []int{-1}, limit: 1_000_000, err: "every dimension requires at least one category"},
		{name: "over the limit", sizes: []int{1000, 1001}, limit: 1_000_000, err: "logical cell count exceeds 1000000"},
		{name: "int64 overflow", sizes: []int{math.MaxInt32, math.MaxInt32, math.MaxInt32}, limit: math.MaxInt64, err: "logical cell count overflows int64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckedProduct(tc.sizes, tc.limit)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("product = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCheckedProductReportsTheCellLimitDistinctly(t *testing.T) {
	_, err := CheckedProduct([]int{2, 3}, 5)
	var limit CellLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("error = %v, want a CellLimitError", err)
	}
	if limit.Limit != 5 {
		t.Fatalf("limit = %d, want 5", limit.Limit)
	}
}

func TestStridesMakeTheLastDimensionVaryFastest(t *testing.T) {
	got := Strides([]int{2, 3, 4})
	want := []int64{12, 4, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("strides = %v, want %v", got, want)
		}
	}
}

func TestCellIndexUsesRowMajorOrder(t *testing.T) {
	sizes := []int{2, 3}
	strides := Strides(sizes)
	cases := []struct {
		coords []int
		index  int64
	}{
		{[]int{0, 0}, 0},
		{[]int{0, 1}, 1},
		{[]int{0, 2}, 2},
		{[]int{1, 0}, 3},
		{[]int{1, 2}, 5},
	}
	for _, tc := range cases {
		got, err := CellIndex(tc.coords, sizes, strides)
		if err != nil {
			t.Fatalf("CellIndex(%v): %v", tc.coords, err)
		}
		if got != tc.index {
			t.Fatalf("CellIndex(%v) = %d, want %d", tc.coords, got, tc.index)
		}
	}
}

func TestCoordinatesAndCellIndexRoundTrip(t *testing.T) {
	sizes := []int{3, 1, 4, 2}
	strides := Strides(sizes)
	count, err := CheckedProduct(sizes, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for index := int64(0); index < count; index++ {
		coords, err := Coordinates(index, sizes, strides)
		if err != nil {
			t.Fatalf("Coordinates(%d): %v", index, err)
		}
		back, err := CellIndex(coords, sizes, strides)
		if err != nil {
			t.Fatalf("CellIndex(%v): %v", coords, err)
		}
		if back != index {
			t.Fatalf("round trip %d -> %v -> %d", index, coords, back)
		}
	}
}

func TestCoordinatesRejectsIndexesOutsideTheCube(t *testing.T) {
	sizes := []int{2, 3}
	strides := Strides(sizes)
	for _, index := range []int64{-1, 6, 7, math.MaxInt64} {
		if _, err := Coordinates(index, sizes, strides); err == nil {
			t.Fatalf("Coordinates(%d) accepted an index outside the cube", index)
		}
	}
}

func TestCellIndexRejectsCoordinatesOutsideTheCube(t *testing.T) {
	sizes := []int{2, 3}
	strides := Strides(sizes)
	for _, coords := range [][]int{{2, 0}, {0, 3}, {-1, 0}, {0}} {
		if _, err := CellIndex(coords, sizes, strides); err == nil {
			t.Fatalf("CellIndex(%v) accepted coordinates outside the cube", coords)
		}
	}
}
