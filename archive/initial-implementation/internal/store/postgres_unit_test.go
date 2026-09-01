package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

var testDimensions = []domain.Dimension{
	{Code: "Sex", Categories: []string{"M", "F"}},
	{Code: "Year", Categories: []string{"2024", "2025"}},
}

func TestCoordinateIndexLastDimensionFastest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		coordinate map[string]string
		want       int64
	}{
		{map[string]string{"Sex": "M", "Year": "2024"}, 0},
		{map[string]string{"year": "2025", "sex": "M"}, 1},
		{map[string]string{"Sex": "F", "Year": "2024"}, 2},
		{map[string]string{"Sex": "F", "Year": "2025"}, 3},
	}
	for _, test := range tests {
		got, err := coordinateIndex(testDimensions, test.coordinate)
		if err != nil || got != test.want {
			t.Fatalf("coordinateIndex(%v) = %d, %v; want %d", test.coordinate, got, err, test.want)
		}
	}
}

func TestPreparePatchRejectsDuplicateCoordinates(t *testing.T) {
	t.Parallel()
	value := json.RawMessage("1")
	observations := []domain.PatchObservation{
		{Categories: map[string]string{"Sex": "M", "Year": "2024"}, Value: value},
		{Categories: map[string]string{"sex": "m", "year": "2024"}, Value: value},
	}
	if _, err := preparePatch(testDimensions, observations); err == nil {
		t.Fatal("expected duplicate coordinates to fail")
	}
}

func TestPreparePatchDeleteShape(t *testing.T) {
	t.Parallel()
	observations := []domain.PatchObservation{{
		Categories: map[string]string{"Sex": "M", "Year": "2024"},
		Delete:     json.RawMessage("true"),
		Value:      json.RawMessage("1"),
	}}
	if _, err := preparePatch(testDimensions, observations); err == nil {
		t.Fatal("expected delete item with a value to fail")
	}
}

func TestPreparePatchRejectsFalseOrNullDelete(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"false", "null"} {
		observations := []domain.PatchObservation{{
			Categories: map[string]string{"Sex": "M", "Year": "2024"},
			Delete:     json.RawMessage(raw),
			Value:      json.RawMessage("1"),
		}}
		if _, err := preparePatch(testDimensions, observations); err == nil {
			t.Fatalf("expected delete=%s to fail", raw)
		}
	}
}

func TestSelectionIndexesReordersDimensions(t *testing.T) {
	t.Parallel()
	requested := []domain.Dimension{
		{Code: "Year", Categories: []string{"2025"}},
		{Code: "Sex", Categories: []string{"F", "M"}},
	}
	canonical, indexes, err := selectionIndexes(testDimensions, requested, 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{3, 1}; !reflect.DeepEqual(indexes, want) {
		t.Fatalf("indexes = %v, want %v", indexes, want)
	}
	if canonical[0].Code != "Year" || canonical[1].Categories[0] != "F" {
		t.Fatalf("unexpected canonical selection: %#v", canonical)
	}
}

func TestParsePGNumericPreservesScientificPrecision(t *testing.T) {
	t.Parallel()
	numeric, err := parsePGNumeric("-12345678901234567890.001e+12")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := numeric.Int.String(), "-12345678901234567890001"; got != want {
		t.Fatalf("integer = %s, want %s", got, want)
	}
	if numeric.Exp != 9 {
		t.Fatalf("exponent = %d, want 9", numeric.Exp)
	}
}
