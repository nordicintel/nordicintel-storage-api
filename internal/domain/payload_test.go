package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const maxCells = int64(1_000_000)

// twoByTwoStructure is the running example from the contract: a payload whose
// dimension and category order deliberately differs from the stored
// normalized-key order, so every test using it exercises payload-to-internal
// remapping rather than an accidental identity mapping.
//
//	payload id     ["year", "sex"], year {2025:0, 2024:1}, sex {M:0, F:1}
//	payload index  0=(2025,M) 1=(2025,F) 2=(2024,M) 3=(2024,F)
//	stored order   sex (F=0, M=1) then year (2024=0, 2025=1)
//	stored index   sex*2 + year
//	remapping      payload 0->3, 1->1, 2->2, 3->0
const twoByTwoStructure = `"id":["year","sex"],"dimension":{"year":{"index":{"2025":0,"2024":1}},"sex":{"index":{"M":0,"F":1}}}`

func replacementBody(fields ...string) []byte {
	return []byte("{" + strings.Join(fields, ",") + "}")
}

func cellsByIndex(t *testing.T, cells []Cell) map[int64]Cell {
	t.Helper()
	byIndex := make(map[int64]Cell, len(cells))
	for i, cell := range cells {
		if i > 0 && cells[i-1].Index >= cell.Index {
			t.Fatalf("cells are not sorted by ascending index: %d then %d", cells[i-1].Index, cell.Index)
		}
		byIndex[cell.Index] = cell
	}
	return byIndex
}

func TestParseReplacementSortsStructureByNormalizedKey(t *testing.T) {
	body := replacementBody(`"source_stamp":{"etag":"abc"}`, twoByTwoStructure, `"value":[1,2,3,4]`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatalf("ParseReplacement: %v", err)
	}
	if replacement.Provider.Key != "scb" || replacement.Provider.Spelling != "SCB" {
		t.Fatalf("provider = %+v", replacement.Provider)
	}
	if replacement.Dataset.Key != "population" || replacement.Dataset.Spelling != "Population" {
		t.Fatalf("dataset = %+v", replacement.Dataset)
	}
	if replacement.CellCount != 4 {
		t.Fatalf("cell count = %d, want 4", replacement.CellCount)
	}
	if len(replacement.Dimensions) != 2 {
		t.Fatalf("dimensions = %d, want 2", len(replacement.Dimensions))
	}
	if replacement.Dimensions[0].Code.Key != "sex" || replacement.Dimensions[0].Position != 0 {
		t.Fatalf("first stored dimension = %+v, want sex at position 0", replacement.Dimensions[0])
	}
	if replacement.Dimensions[1].Code.Key != "year" || replacement.Dimensions[1].Position != 1 {
		t.Fatalf("second stored dimension = %+v, want year at position 1", replacement.Dimensions[1])
	}
	sex := replacement.Dimensions[0].Categories
	if sex[0].Code.Spelling != "F" || sex[0].Position != 0 || sex[1].Code.Spelling != "M" || sex[1].Position != 1 {
		t.Fatalf("sex categories = %+v, want F then M", sex)
	}
	year := replacement.Dimensions[1].Categories
	if year[0].Code.Spelling != "2024" || year[1].Code.Spelling != "2025" {
		t.Fatalf("year categories = %+v, want 2024 then 2025", year)
	}
}

func TestParseReplacementRemapsDensePayloadIndexesToInternalIndexes(t *testing.T) {
	body := replacementBody(`"source_stamp":null`, twoByTwoStructure, `"value":[10,20,30,40]`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatalf("ParseReplacement: %v", err)
	}
	cells := cellsByIndex(t, replacement.Cells)
	want := map[int64]float64{3: 10, 1: 20, 2: 30, 0: 40}
	if len(cells) != len(want) {
		t.Fatalf("stored %d cells, want %d", len(cells), len(want))
	}
	for index, value := range want {
		cell, exists := cells[index]
		if !exists || cell.Numeric == nil || *cell.Numeric != value {
			t.Fatalf("internal index %d = %+v, want numeric %v", index, cell, value)
		}
	}
	if replacement.ValuedCount != 4 {
		t.Fatalf("valued count = %d, want 4", replacement.ValuedCount)
	}
}

func TestParseReplacementRemapsSparsePayloadIndexes(t *testing.T) {
	body := replacementBody(`"source_stamp":1`, twoByTwoStructure, `"value":{"0":10.5,"3":40}`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatalf("ParseReplacement: %v", err)
	}
	cells := cellsByIndex(t, replacement.Cells)
	if len(cells) != 2 {
		t.Fatalf("stored %d cells, want 2", len(cells))
	}
	if cell := cells[3]; cell.Numeric == nil || *cell.Numeric != 10.5 {
		t.Fatalf("payload index 0 should map to internal index 3, got %+v", cells)
	}
	if cell := cells[0]; cell.Numeric == nil || *cell.Numeric != 40 {
		t.Fatalf("payload index 3 should map to internal index 0, got %+v", cells)
	}
	if replacement.ValuedCount != 2 {
		t.Fatalf("valued count = %d, want 2", replacement.ValuedCount)
	}
}

func TestParseReplacementStoresOnlyPopulatedCells(t *testing.T) {
	body := replacementBody(`"source_stamp":null`, twoByTwoStructure,
		`"value":[1,null,null,null]`, `"text":[null,null,"confidential",null]`, `"status":[null,"a",null,null]`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatalf("ParseReplacement: %v", err)
	}
	cells := cellsByIndex(t, replacement.Cells)
	if len(cells) != 3 {
		t.Fatalf("stored %d cells, want 3 populated cells", len(cells))
	}
	if cell := cells[3]; cell.Numeric == nil || *cell.Numeric != 1 {
		t.Fatalf("payload index 0 (numeric) mapped incorrectly: %+v", cells)
	}
	if cell := cells[2]; cell.Text == nil || *cell.Text != "confidential" {
		t.Fatalf("payload index 2 (text) mapped incorrectly: %+v", cells)
	}
	if cell := cells[1]; cell.Status == nil || *cell.Status != "a" {
		t.Fatalf("payload index 1 (status) mapped incorrectly: %+v", cells)
	}
	if replacement.ValuedCount != 2 {
		t.Fatalf("valued count = %d, want 2; a status-only cell is not valued", replacement.ValuedCount)
	}
}

func TestParseReplacementExpandsScalarStatusAcrossEveryCell(t *testing.T) {
	body := replacementBody(`"source_stamp":null`, twoByTwoStructure, `"value":{}`, `"status":"c"`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatalf("ParseReplacement: %v", err)
	}
	if len(replacement.Cells) != 4 {
		t.Fatalf("stored %d cells, want a status row for every logical cell", len(replacement.Cells))
	}
	for _, cell := range replacement.Cells {
		if cell.Status == nil || *cell.Status != "c" {
			t.Fatalf("cell %d = %+v, want status c", cell.Index, cell)
		}
		if cell.Numeric != nil || cell.Text != nil {
			t.Fatalf("cell %d unexpectedly carries a value", cell.Index)
		}
	}
	if replacement.ValuedCount != 0 {
		t.Fatalf("valued count = %d, want 0", replacement.ValuedCount)
	}
}

func TestParseReplacementAcceptsTextOnlyDatasets(t *testing.T) {
	body := replacementBody(`"source_stamp":null`, twoByTwoStructure,
		`"value":[null,null,null,null]`, `"text":["a","b","c","d"]`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatalf("ParseReplacement: %v", err)
	}
	if replacement.ValuedCount != 4 {
		t.Fatalf("valued count = %d, want 4", replacement.ValuedCount)
	}
	for _, cell := range replacement.Cells {
		if cell.Text == nil || cell.Numeric != nil {
			t.Fatalf("cell %d = %+v, want a text-only cell", cell.Index, cell)
		}
	}
}

func TestParseReplacementPreservesSourceStampSemantically(t *testing.T) {
	for _, stamp := range []string{`null`, `{"etag":"abc"}`, `"plain"`, `12`, `[1,2]`, `true`} {
		body := replacementBody(`"source_stamp":`+stamp, twoByTwoStructure, `"value":{}`)
		replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
		if err != nil {
			t.Fatalf("source_stamp %s: %v", stamp, err)
		}
		var got, want any
		if err := json.Unmarshal(replacement.SourceStamp, &got); err != nil {
			t.Fatalf("stored stamp %q is not JSON: %v", replacement.SourceStamp, err)
		}
		if err := json.Unmarshal([]byte(stamp), &want); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("stamp %s round-tripped to %v", stamp, got)
		}
	}
}

func TestParseReplacementRejectsInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed", `{`},
		{"duplicate top-level key", `{"source_stamp":null,"source_stamp":1,` + twoByTwoStructure + `,"value":{}}`},
		{"duplicate nested source stamp key", `{"source_stamp":{"a":1,"a":2},` + twoByTwoStructure + `,"value":{}}`},
		{"trailing value", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{}} 7`},
		{"unknown field", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{},"extra":1}`},
		{"not an object", `[1,2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReplacement("SCB", "Population", []byte(tc.body), maxCells)
			if !errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("error = %v, want ErrInvalidJSON", err)
			}
		})
	}
}

func TestParseReplacementRejectsInvalidStructures(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing source stamp", `{` + twoByTwoStructure + `,"value":{}}`},
		{"missing value", `{"source_stamp":null,` + twoByTwoStructure + `}`},
		{"no dimensions", `{"source_stamp":null,"id":[],"dimension":{},"value":{}}`},
		{"empty dimension", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{}}},"value":{}}`},
		{"id not in dimension", `{"source_stamp":null,"id":["a"],"dimension":{"b":{"index":{"x":0}}},"value":[1]}`},
		{"more dimensions than ids", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}},"b":{"index":{"y":0}}},"value":[1]}`},
		{"duplicate id entry", `{"source_stamp":null,"id":["a","A"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`},
		{"colliding dimension codes", `{"source_stamp":null,"id":["a","B"],"dimension":{"a":{"index":{"x":0}},"A":{"index":{"y":0}}},"value":[1]}`},
		{"colliding category codes", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0,"X":1}}},"value":[1,2]}`},
		{"non zero-based category index", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":1}}},"value":[1]}`},
		{"non contiguous category indexes", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0,"y":2}}},"value":[1,2]}`},
		{"duplicate category index", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0,"y":0}}},"value":[1,2]}`},
		{"negative category index", `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":-1}}},"value":[1]}`},
		{"empty dimension code", `{"source_stamp":null,"id":["  "],"dimension":{"  ":{"index":{"x":0}}},"value":[1]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReplacement("SCB", "Population", []byte(tc.body), maxCells)
			if err == nil {
				t.Fatal("invalid structure accepted")
			}
			if errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("error = %v, want a semantic validation error rather than a JSON error", err)
			}
		})
	}
}

func TestParseReplacementBoundsTheDimensionCount(t *testing.T) {
	build := func(count int) []byte {
		ids := make([]string, count)
		dims := make([]string, count)
		for i := range ids {
			name := fmt.Sprintf("d%d", i)
			ids[i] = `"` + name + `"`
			dims[i] = `"` + name + `":{"index":{"x":0}}`
		}
		return []byte(`{"source_stamp":null,"id":[` + strings.Join(ids, ",") +
			`],"dimension":{` + strings.Join(dims, ",") + `},"value":[1]}`)
	}
	if _, err := ParseReplacement("SCB", "Population", build(MaxDimensions), maxCells); err != nil {
		t.Fatalf("%d dimensions were rejected: %v", MaxDimensions, err)
	}
	if _, err := ParseReplacement("SCB", "Population", build(MaxDimensions+1), maxCells); err == nil {
		t.Fatalf("%d dimensions were accepted", MaxDimensions+1)
	}
}

func TestParseReplacementRejectsInvalidChannels(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"dense value too short", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1,2,3]}`},
		{"dense value too long", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1,2,3,4,5]}`},
		{"value is a scalar", `{"source_stamp":null,` + twoByTwoStructure + `,"value":1}`},
		{"value is a string", `{"source_stamp":null,` + twoByTwoStructure + `,"value":"x"}`},
		{"non numeric dense entry", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1,2,3,"x"]}`},
		{"non numeric sparse entry", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"0":"x"}}`},
		{"sparse explicit null", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"0":null}}`},
		{"sparse index out of range", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"4":1}}`},
		{"sparse index negative", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"-1":1}}`},
		{"sparse index not canonical", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"01":1}}`},
		{"sparse index not an integer", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"a":1}}`},
		{"sparse index is a float", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"1.0":1}}`},
		{"numeric overflows float64", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1e400,null,null,null]}`},
		{"text representation differs from value", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"0":1},"text":[null,null,null,"x"]}`},
		{"status representation differs from value", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1,2,3,4],"status":{"0":"a"}}`},
		{"dense text wrong length", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1,2,3,4],"text":[null]}`},
		{"text entry is not a string", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[null,null,null,null],"text":[1,null,null,null]}`},
		{"sparse text explicit null", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{},"text":{"0":null}}`},
		{"numeric and text collide", `{"source_stamp":null,` + twoByTwoStructure + `,"value":[1,null,null,null],"text":["x",null,null,null]}`},
		{"sparse numeric and text collide", `{"source_stamp":null,` + twoByTwoStructure + `,"value":{"2":1},"text":{"2":"x"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReplacement("SCB", "Population", []byte(tc.body), maxCells); err == nil {
				t.Fatal("invalid channel accepted")
			}
		})
	}
}

func TestParseReplacementRejectsCellCountsOverTheLimit(t *testing.T) {
	body := `{"source_stamp":null,"id":["a","b"],"dimension":{"a":{"index":{"x":0,"y":1}},"b":{"index":{"p":0,"q":1}}},"value":{}}`
	_, err := ParseReplacement("SCB", "Population", []byte(body), 3)
	var limit CellLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("error = %v, want a CellLimitError", err)
	}
}

func TestParseReplacementRejectsInvalidPathCodes(t *testing.T) {
	body := replacementBody(`"source_stamp":null`, twoByTwoStructure, `"value":{}`)
	if _, err := ParseReplacement("   ", "Population", body, maxCells); err == nil {
		t.Fatal("empty provider code accepted")
	}
	if _, err := ParseReplacement("SCB", strings.Repeat("a", MaxCodeBytes+1), body, maxCells); err == nil {
		t.Fatal("oversized dataset code accepted")
	}
}

func TestParseReplacementDefaultsReplaceToFalse(t *testing.T) {
	body := replacementBody(`"source_stamp":null`, twoByTwoStructure, `"value":{}`)
	replacement, err := ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Replace {
		t.Fatal("replace defaulted to true")
	}
	body = replacementBody(`"replace":true`, `"source_stamp":null`, twoByTwoStructure, `"value":{}`)
	replacement, err = ParseReplacement("SCB", "Population", body, maxCells)
	if err != nil {
		t.Fatal(err)
	}
	if !replacement.Replace {
		t.Fatal("replace:true was not honoured")
	}
}

func TestParseSelectionKeepsRequestedOrder(t *testing.T) {
	selection, err := ParseSelection([]byte(`{"id":["year","sex"],"dimension":{"year":{"index":{"2025":0}},"sex":{"index":{"F":0,"M":1}}}}`))
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	if len(selection.Dimensions) != 2 {
		t.Fatalf("dimensions = %d, want 2", len(selection.Dimensions))
	}
	if selection.Dimensions[0].Code.Key != "year" || selection.Dimensions[1].Code.Key != "sex" {
		t.Fatalf("selection order = %q then %q, want the requested order",
			selection.Dimensions[0].Code.Key, selection.Dimensions[1].Code.Key)
	}
	if len(selection.Dimensions[0].Categories) != 1 || selection.Dimensions[0].Categories[0].Key != "2025" {
		t.Fatalf("year categories = %+v", selection.Dimensions[0].Categories)
	}
	if selection.Dimensions[1].Categories[0].Key != "f" || selection.Dimensions[1].Categories[1].Key != "m" {
		t.Fatalf("sex categories = %+v, want the requested F then M order", selection.Dimensions[1].Categories)
	}
}

func TestParseSelectionRejectsObservationChannels(t *testing.T) {
	for _, body := range []string{
		`{"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`,
		`{"id":["a"],"dimension":{"a":{"index":{"x":0}}},"status":"c"}`,
		`{"id":["a"],"dimension":{"a":{"index":{"x":0}}},"replace":true}`,
	} {
		if _, err := ParseSelection([]byte(body)); !errors.Is(err, ErrInvalidJSON) {
			t.Fatalf("body %s: error = %v, want ErrInvalidJSON for an unknown field", body, err)
		}
	}
}

func TestParseSelectionRejectsInvalidSelectors(t *testing.T) {
	for _, body := range []string{
		`{"id":[],"dimension":{}}`,
		`{"id":["a"],"dimension":{"a":{"index":{}}}}`,
		`{"id":["a"],"dimension":{"b":{"index":{"x":0}}}}`,
		`{"id":["a"],"dimension":{"a":{"index":{"x":1}}}}`,
	} {
		if _, err := ParseSelection([]byte(body)); err == nil {
			t.Fatalf("body %s was accepted", body)
		}
	}
}
