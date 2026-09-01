package domain

import (
	"encoding/json"
	"time"
)

const (
	MaxCodeBytes  = 256
	MaxDimensions = 64
)

type Code struct {
	Spelling string
	Key      string
}

type Category struct {
	Code     Code
	Position int
}

type Dimension struct {
	Code       Code
	Position   int
	Categories []Category
}

type Cell struct {
	Index   int64
	Numeric *float64
	Text    *string
	Status  *string
}

type Summary struct {
	ProviderCode    string          `json:"provider_code"`
	DatasetCode     string          `json:"dataset_code"`
	SourceStamp     json.RawMessage `json:"source_stamp"`
	CellCount       int64           `json:"cell_count"`
	ValuedCellCount int64           `json:"valued_cell_count"`
	NullCellCount   int64           `json:"null_cell_count"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type ProviderListItem struct {
	ProviderCode string `json:"provider_code"`
	DatasetCount int64  `json:"dataset_count"`
}

type Dataset struct {
	Summary    Summary
	Dimensions []Dimension
	Cells      []Cell
}

type Replacement struct {
	Provider    Code
	Dataset     Code
	Replace     bool
	SourceStamp json.RawMessage
	CellCount   int64
	ValuedCount int64
	Dimensions  []Dimension
	Cells       []Cell
}

type SelectionDimension struct {
	Code       Code
	Categories []Code
}

type Selection struct {
	Dimensions []SelectionDimension
}

type View struct {
	Summary    Summary
	Dimensions []Dimension
	Cells      []Cell
}
