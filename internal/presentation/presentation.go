package presentation

import (
	"fmt"
	"strconv"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

type Dimension struct {
	Index map[string]int `json:"index"`
}

type Structure struct {
	ProviderCode    string               `json:"provider_code"`
	DatasetCode     string               `json:"dataset_code"`
	SourceStamp     any                  `json:"source_stamp"`
	CellCount       int64                `json:"cell_count"`
	ValuedCellCount int64                `json:"valued_cell_count"`
	NullCellCount   int64                `json:"null_cell_count"`
	UpdatedAt       any                  `json:"updated_at"`
	ID              []string             `json:"id"`
	Dimension       map[string]Dimension `json:"dimension"`
}

type Data struct {
	ProviderCode    string               `json:"provider_code"`
	DatasetCode     string               `json:"dataset_code"`
	SourceStamp     any                  `json:"source_stamp"`
	CellCount       int64                `json:"cell_count"`
	ValuedCellCount int64                `json:"valued_cell_count"`
	NullCellCount   int64                `json:"null_cell_count"`
	UpdatedAt       any                  `json:"updated_at"`
	ID              []string             `json:"id"`
	Dimension       map[string]Dimension `json:"dimension"`
	Value           any                  `json:"value"`
	Text            any                  `json:"text,omitempty"`
	Status          any                  `json:"status,omitempty"`
}

func StructureResponse(view domain.View) (Structure, error) {
	base, err := structure(view)
	if err != nil {
		return Structure{}, err
	}
	return base, nil
}

func DataResponse(view domain.View, format string) (Data, error) {
	base, err := structure(view)
	if err != nil {
		return Data{}, err
	}
	sizes := make([]int, len(view.Dimensions))
	for i, dimension := range view.Dimensions {
		sizes[i] = len(dimension.Categories)
	}
	logicalCount, err := domain.CheckedProduct(sizes, domainMaxCellCount)
	if err != nil {
		return Data{}, err
	}
	response := Data{
		ProviderCode: base.ProviderCode, DatasetCode: base.DatasetCode,
		SourceStamp: base.SourceStamp, CellCount: base.CellCount,
		ValuedCellCount: base.ValuedCellCount, NullCellCount: base.NullCellCount,
		UpdatedAt: base.UpdatedAt, ID: base.ID, Dimension: base.Dimension,
	}
	if format == "dense" {
		values := make([]*float64, logicalCount)
		var texts []*string
		var statuses []*string
		hasText := false
		hasStatus := false
		for _, cell := range view.Cells {
			if cell.Index < 0 || cell.Index >= logicalCount {
				return Data{}, fmt.Errorf("stored cell index is outside the response cube")
			}
			values[cell.Index] = cell.Numeric
			if cell.Text != nil {
				if texts == nil {
					texts = make([]*string, logicalCount)
				}
				texts[cell.Index] = cell.Text
				hasText = true
			}
			if cell.Status != nil {
				if statuses == nil {
					statuses = make([]*string, logicalCount)
				}
				statuses[cell.Index] = cell.Status
				hasStatus = true
			}
		}
		response.Value = values
		if hasText {
			response.Text = texts
		}
		if scalar, ok := sharedStatus(view.Cells, logicalCount); ok {
			response.Status = scalar
		} else if hasStatus {
			response.Status = statuses
		}
		return response, nil
	}

	values := make(map[string]float64)
	texts := make(map[string]string)
	statuses := make(map[string]string)
	for _, cell := range view.Cells {
		if cell.Index < 0 || cell.Index >= logicalCount {
			return Data{}, fmt.Errorf("stored cell index is outside the response cube")
		}
		key := strconv.FormatInt(cell.Index, 10)
		if cell.Numeric != nil {
			values[key] = *cell.Numeric
		}
		if cell.Text != nil {
			texts[key] = *cell.Text
		}
		if cell.Status != nil {
			statuses[key] = *cell.Status
		}
	}
	response.Value = values
	if len(texts) > 0 {
		response.Text = texts
	}
	if scalar, ok := sharedStatus(view.Cells, logicalCount); ok {
		response.Status = scalar
	} else if len(statuses) > 0 {
		response.Status = statuses
	}
	return response, nil
}

const domainMaxCellCount = int64(1_000_000)

func structure(view domain.View) (Structure, error) {
	response := Structure{
		ProviderCode: view.Summary.ProviderCode, DatasetCode: view.Summary.DatasetCode,
		SourceStamp: view.Summary.SourceStamp, CellCount: view.Summary.CellCount,
		ValuedCellCount: view.Summary.ValuedCellCount, NullCellCount: view.Summary.NullCellCount,
		UpdatedAt: view.Summary.UpdatedAt, ID: make([]string, len(view.Dimensions)),
		Dimension: make(map[string]Dimension, len(view.Dimensions)),
	}
	for i, dimension := range view.Dimensions {
		response.ID[i] = dimension.Code.Spelling
		index := make(map[string]int, len(dimension.Categories))
		for _, category := range dimension.Categories {
			index[category.Code.Spelling] = category.Position
		}
		response.Dimension[dimension.Code.Spelling] = Dimension{Index: index}
	}
	return response, nil
}

func sharedStatus(cells []domain.Cell, logicalCount int64) (string, bool) {
	if int64(len(cells)) < logicalCount || logicalCount == 0 {
		return "", false
	}
	var shared string
	for i, cell := range cells {
		if cell.Status == nil {
			return "", false
		}
		if i == 0 {
			shared = *cell.Status
		} else if *cell.Status != shared {
			return "", false
		}
	}
	return shared, true
}
