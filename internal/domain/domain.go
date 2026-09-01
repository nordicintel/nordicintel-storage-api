package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

type Dimension struct {
	Code       string   `json:"code"`
	Categories []string `json:"categories"`
}

type PutRequest struct {
	SourceStamp json.RawMessage   `json:"source_stamp,omitempty"`
	Dimensions  []Dimension       `json:"dimensions"`
	Values      []json.RawMessage `json:"values"`
	Statuses    json.RawMessage   `json:"statuses,omitempty"`
}

type PatchRequest struct {
	SourceStamp  json.RawMessage    `json:"source_stamp,omitempty"`
	Observations []PatchObservation `json:"observations"`
}

type PatchObservation struct {
	Categories Coordinate      `json:"categories"`
	Value      json.RawMessage `json:"value,omitempty"`
	StatusCode json.RawMessage `json:"status_code,omitempty"`
	Delete     json.RawMessage `json:"delete,omitempty"`
}

type Coordinate map[string]string

func (c *Coordinate) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return errors.New("must be an object of dimension and category strings")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("must be an object of dimension and category strings")
	}
	result := make(Coordinate)
	normalizedKeys := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return errors.New("must be an object of dimension and category strings")
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("must contain string dimension keys")
		}
		normalized := NormalizeCode(key)
		if _, exists := normalizedKeys[normalized]; exists {
			return fmt.Errorf("contains duplicate normalized dimension code %q", key)
		}
		normalizedKeys[normalized] = struct{}{}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("category for dimension %q must be a string", key)
		}
		result[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return errors.New("must be an object of dimension and category strings")
	}
	if err := decoder.Decode(&token); !errors.Is(err, io.EOF) {
		return errors.New("must contain one object")
	}
	*c = result
	return nil
}

type SelectionRequest struct {
	Dimensions []Dimension `json:"dimensions"`
}

type Cell struct {
	Index      int64
	Value      *Decimal
	StatusCode *string
}

type PatchCell struct {
	Index      int64
	Value      *Decimal
	StatusCode *string
	Delete     bool
}

type DatasetMetadata struct {
	ProviderCode          string          `json:"provider_code"`
	DatasetCode           string          `json:"dataset_code"`
	ObservationCount      int64           `json:"observation_count"`
	ObservationsUpdatedAt *time.Time      `json:"observations_updated_at"`
	SourceStamp           json.RawMessage `json:"source_stamp"`
	Dimensions            []Dimension     `json:"dimensions,omitempty"`
}

type ProviderSummary struct {
	ProviderCode string `json:"provider_code"`
	DatasetCount int64  `json:"dataset_count"`
}

type DatasetSummary struct {
	ProviderCode          string          `json:"provider_code"`
	DatasetCode           string          `json:"dataset_code"`
	ObservationCount      int64           `json:"observation_count"`
	ObservationsUpdatedAt *time.Time      `json:"observations_updated_at"`
	SourceStamp           json.RawMessage `json:"source_stamp,omitempty"`
	Dimensions            []Dimension     `json:"dimensions,omitempty"`
}

type Decimal string

func (d Decimal) MarshalJSON() ([]byte, error) {
	b := []byte(d)
	if !validJSONNumber(b) {
		return nil, fmt.Errorf("invalid decimal %q", d)
	}
	return b, nil
}

func ParseDecimal(raw json.RawMessage) (*Decimal, error) {
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if !validJSONNumber(raw) {
		return nil, errors.New("must be a JSON number or null")
	}
	d := Decimal(string(raw))
	return &d, nil
}

func ParseNullableString(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("must be a string or null")
	}
	return &value, nil
}

func NormalizeCode(value string) string {
	return norm.NFKC.String(cases.Lower(language.Und).String(norm.NFKC.String(strings.Trim(value, " "))))
}

func ValidateDimensions(dimensions []Dimension, maxCells int64) (int64, error) {
	if len(dimensions) == 0 {
		return 0, errors.New("dimensions must be a non-empty array")
	}
	product := int64(1)
	dimensionKeys := make(map[string]struct{}, len(dimensions))
	for i, dimension := range dimensions {
		key := NormalizeCode(dimension.Code)
		if key == "" {
			return 0, fmt.Errorf("dimensions[%d].code must be non-empty", i)
		}
		if _, exists := dimensionKeys[key]; exists {
			return 0, fmt.Errorf("duplicate dimension code %q", dimension.Code)
		}
		dimensionKeys[key] = struct{}{}
		if len(dimension.Categories) == 0 {
			return 0, fmt.Errorf("dimension %q must have at least one category", dimension.Code)
		}
		categoryKeys := make(map[string]struct{}, len(dimension.Categories))
		for _, category := range dimension.Categories {
			categoryKey := NormalizeCode(category)
			if categoryKey == "" {
				return 0, fmt.Errorf("dimension %q contains an empty category code", dimension.Code)
			}
			if _, exists := categoryKeys[categoryKey]; exists {
				return 0, fmt.Errorf("dimension %q contains duplicate category %q", dimension.Code, category)
			}
			categoryKeys[categoryKey] = struct{}{}
		}
		size := int64(len(dimension.Categories))
		if product > math.MaxInt64/size || product*size > maxCells {
			return 0, fmt.Errorf("dataset cube exceeds the %d cell limit", maxCells)
		}
		product *= size
	}
	return product, nil
}

func validJSONNumber(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return false
	}
	var extra any
	return decoder.Decode(&extra) != nil && strings.TrimSpace(string(raw)) == number.String()
}
