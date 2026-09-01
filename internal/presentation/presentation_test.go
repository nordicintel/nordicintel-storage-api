package presentation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

func numeric(value float64) *float64 { return &value }
func text(value string) *string      { return &value }

func exampleView(cells ...domain.Cell) domain.View {
	return domain.View{
		Summary: domain.Summary{
			ProviderCode: "SCB", DatasetCode: "Population",
			SourceStamp:     json.RawMessage(`{"etag":"abc"}`),
			CellCount:       4,
			ValuedCellCount: 2,
			NullCellCount:   2,
			UpdatedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
		Dimensions: []domain.Dimension{
			{Code: domain.Code{Spelling: "sex", Key: "sex"}, Position: 0, Categories: []domain.Category{
				{Code: domain.Code{Spelling: "F", Key: "f"}, Position: 0},
				{Code: domain.Code{Spelling: "M", Key: "m"}, Position: 1},
			}},
			{Code: domain.Code{Spelling: "year", Key: "year"}, Position: 1, Categories: []domain.Category{
				{Code: domain.Code{Spelling: "2024", Key: "2024"}, Position: 0},
				{Code: domain.Code{Spelling: "2025", Key: "2025"}, Position: 1},
			}},
		},
		Cells: cells,
	}
}

func encode(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

func TestStructureResponseCarriesMetadataAndStoredSpellings(t *testing.T) {
	response, err := StructureResponse(exampleView())
	if err != nil {
		t.Fatalf("StructureResponse: %v", err)
	}
	decoded := encode(t, response)
	for _, field := range []string{"provider_code", "dataset_code", "source_stamp",
		"cell_count", "valued_cell_count", "null_cell_count", "updated_at", "id", "dimension"} {
		if _, present := decoded[field]; !present {
			t.Fatalf("structure response is missing %q: %v", field, decoded)
		}
	}
	ids := decoded["id"].([]any)
	if len(ids) != 2 || ids[0] != "sex" || ids[1] != "year" {
		t.Fatalf("id = %v, want [sex year]", ids)
	}
	sex := decoded["dimension"].(map[string]any)["sex"].(map[string]any)["index"].(map[string]any)
	if sex["F"] != float64(0) || sex["M"] != float64(1) {
		t.Fatalf("sex index = %v, want F:0 M:1", sex)
	}
	if _, hasValue := decoded["value"]; hasValue {
		t.Fatal("structure response must not carry observation channels")
	}
}

func TestDenseResponseReturnsEveryLogicalCell(t *testing.T) {
	view := exampleView(
		domain.Cell{Index: 0, Numeric: numeric(10.5)},
		domain.Cell{Index: 3, Text: text("confidential"), Status: text("c")},
	)
	response, err := DataResponse(view, "dense")
	if err != nil {
		t.Fatalf("DataResponse: %v", err)
	}
	decoded := encode(t, response)
	values := decoded["value"].([]any)
	if len(values) != 4 {
		t.Fatalf("value = %v, want four entries", values)
	}
	if values[0] != 10.5 || values[1] != nil || values[2] != nil || values[3] != nil {
		t.Fatalf("value = %v", values)
	}
	texts := decoded["text"].([]any)
	if len(texts) != 4 || texts[3] != "confidential" || texts[0] != nil {
		t.Fatalf("text = %v", texts)
	}
	statuses := decoded["status"].([]any)
	if len(statuses) != 4 || statuses[3] != "c" || statuses[0] != nil {
		t.Fatalf("status = %v", statuses)
	}
}

func TestSparseResponseOmitsInferredNullCells(t *testing.T) {
	view := exampleView(
		domain.Cell{Index: 0, Numeric: numeric(10.5)},
		domain.Cell{Index: 3, Text: text("confidential"), Status: text("c")},
	)
	response, err := DataResponse(view, "sparse")
	if err != nil {
		t.Fatalf("DataResponse: %v", err)
	}
	decoded := encode(t, response)
	values := decoded["value"].(map[string]any)
	if len(values) != 1 || values["0"] != 10.5 {
		t.Fatalf("value = %v, want only index 0", values)
	}
	texts := decoded["text"].(map[string]any)
	if len(texts) != 1 || texts["3"] != "confidential" {
		t.Fatalf("text = %v", texts)
	}
	statuses := decoded["status"].(map[string]any)
	if len(statuses) != 1 || statuses["3"] != "c" {
		t.Fatalf("status = %v", statuses)
	}
}

func TestValueIsAlwaysReturnedAndEmptyChannelsAreOmitted(t *testing.T) {
	t.Run("sparse", func(t *testing.T) {
		response, err := DataResponse(exampleView(), "sparse")
		if err != nil {
			t.Fatalf("DataResponse: %v", err)
		}
		decoded := encode(t, response)
		values, present := decoded["value"]
		if !present {
			t.Fatal("value must always be returned")
		}
		if len(values.(map[string]any)) != 0 {
			t.Fatalf("value = %v, want an empty object", values)
		}
		if _, present := decoded["text"]; present {
			t.Fatal("an all-null text channel must be omitted")
		}
		if _, present := decoded["status"]; present {
			t.Fatal("an all-null status channel must be omitted")
		}
	})
	t.Run("dense", func(t *testing.T) {
		response, err := DataResponse(exampleView(), "dense")
		if err != nil {
			t.Fatalf("DataResponse: %v", err)
		}
		decoded := encode(t, response)
		values := decoded["value"].([]any)
		if len(values) != 4 {
			t.Fatalf("value = %v, want four null entries", values)
		}
		for i, value := range values {
			if value != nil {
				t.Fatalf("value[%d] = %v, want null", i, value)
			}
		}
		if _, present := decoded["text"]; present {
			t.Fatal("an all-null text channel must be omitted")
		}
		if _, present := decoded["status"]; present {
			t.Fatal("an all-null status channel must be omitted")
		}
	})
}

func TestStatusCollapsesToAScalarOnlyWhenEveryCellSharesIt(t *testing.T) {
	shared := []domain.Cell{
		{Index: 0, Status: text("a")},
		{Index: 1, Status: text("a")},
		{Index: 2, Status: text("a")},
		{Index: 3, Status: text("a")},
	}
	for _, format := range []string{"dense", "sparse"} {
		response, err := DataResponse(exampleView(shared...), format)
		if err != nil {
			t.Fatalf("DataResponse(%s): %v", format, err)
		}
		if decoded := encode(t, response); decoded["status"] != "a" {
			t.Fatalf("%s status = %v, want the scalar \"a\"", format, decoded["status"])
		}
	}

	differing := []domain.Cell{
		{Index: 0, Status: text("a")},
		{Index: 1, Status: text("a")},
		{Index: 2, Status: text("a")},
		{Index: 3, Status: text("b")},
	}
	response, err := DataResponse(exampleView(differing...), "sparse")
	if err != nil {
		t.Fatalf("DataResponse: %v", err)
	}
	if _, isScalar := encode(t, response)["status"].(string); isScalar {
		t.Fatal("differing statuses must not collapse to a scalar")
	}

	partial := []domain.Cell{
		{Index: 0, Status: text("a")},
		{Index: 1, Status: text("a")},
		{Index: 2, Status: text("a")},
	}
	response, err = DataResponse(exampleView(partial...), "sparse")
	if err != nil {
		t.Fatalf("DataResponse: %v", err)
	}
	if _, isScalar := encode(t, response)["status"].(string); isScalar {
		t.Fatal("a status covering only some cells must not collapse to a scalar")
	}
}

func TestNumericValuesEncodeAsJSONNumbers(t *testing.T) {
	view := exampleView(
		domain.Cell{Index: 0, Numeric: numeric(0.1)},
		domain.Cell{Index: 1, Numeric: numeric(1e300)},
		domain.Cell{Index: 2, Numeric: numeric(-0)},
		domain.Cell{Index: 3, Numeric: numeric(9007199254740993)},
	)
	response, err := DataResponse(view, "dense")
	if err != nil {
		t.Fatalf("DataResponse: %v", err)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Value []float64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the encoded numbers are not valid JSON numbers: %v", err)
	}
	want := []float64{0.1, 1e300, 0, 9007199254740993}
	for i := range want {
		if decoded.Value[i] != want[i] {
			t.Fatalf("value[%d] = %v, want %v", i, decoded.Value[i], want[i])
		}
	}
}

func TestSubsetResponsesKeepWholeDatasetCounts(t *testing.T) {
	view := exampleView(domain.Cell{Index: 0, Numeric: numeric(1)})
	// A subset view carries fewer dimensions than the stored dataset but the
	// summary still describes the complete dataset.
	view.Dimensions = view.Dimensions[:1]
	response, err := DataResponse(view, "sparse")
	if err != nil {
		t.Fatalf("DataResponse: %v", err)
	}
	decoded := encode(t, response)
	if decoded["cell_count"] != float64(4) || decoded["valued_cell_count"] != float64(2) || decoded["null_cell_count"] != float64(2) {
		t.Fatalf("subset counts = %v, want the whole-dataset counts", decoded)
	}
}

func TestDataResponseRejectsCellsOutsideTheResponseCube(t *testing.T) {
	view := exampleView(domain.Cell{Index: 4, Numeric: numeric(1)})
	for _, format := range []string{"dense", "sparse"} {
		if _, err := DataResponse(view, format); err == nil {
			t.Fatalf("%s response accepted a cell outside the cube", format)
		}
	}
}
