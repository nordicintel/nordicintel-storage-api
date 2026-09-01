package domain

import (
	"encoding/json"
	"testing"
)

func TestNormalizeCode(t *testing.T) {
	t.Parallel()
	if got, want := NormalizeCode("  Ｓtraße  "), "straße"; got != want {
		t.Fatalf("NormalizeCode() = %q, want %q", got, want)
	}
}

func TestValidateDimensions(t *testing.T) {
	t.Parallel()
	dimensions := []Dimension{
		{Code: "Sex", Categories: []string{"M", "F"}},
		{Code: "Year", Categories: []string{"2024", "2025"}},
	}
	if got, err := ValidateDimensions(dimensions, 100); err != nil || got != 4 {
		t.Fatalf("ValidateDimensions() = %d, %v; want 4, nil", got, err)
	}
	dimensions[1].Code = " sex "
	if _, err := ValidateDimensions(dimensions, 100); err == nil {
		t.Fatal("expected normalized duplicate dimension to fail")
	}
}

func TestValidateDimensionsLimit(t *testing.T) {
	t.Parallel()
	dimensions := []Dimension{{Code: "A", Categories: []string{"1", "2"}}, {Code: "B", Categories: []string{"1", "2"}}}
	if _, err := ValidateDimensions(dimensions, 3); err == nil {
		t.Fatal("expected cube limit failure")
	}
}

func TestDecimalPreservesJSONToken(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"12345678901234567890.0001", "-1.25e+9", "0"} {
		value, err := ParseDecimal(json.RawMessage(input))
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", input, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", input, err)
		}
		if string(encoded) != input {
			t.Fatalf("round trip = %q, want %q", encoded, input)
		}
	}
	if _, err := ParseDecimal(json.RawMessage(`"1.2"`)); err == nil {
		t.Fatal("expected quoted decimal to fail")
	}
}

func TestCoordinateRejectsNormalizedDuplicateKeys(t *testing.T) {
	t.Parallel()
	var observation PatchObservation
	err := json.Unmarshal([]byte(`{"categories":{"Sex":"M"," sex ":"F"},"value":1}`), &observation)
	if err == nil {
		t.Fatal("expected duplicate normalized coordinate keys to fail")
	}
}
