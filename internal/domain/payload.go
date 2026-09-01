package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/nordicintel/nordicintel-storage-api/internal/jsonx"
)

var ErrInvalidJSON = errors.New("invalid JSON")

type dimensionPayload struct {
	Index map[string]int `json:"index"`
}

type replacementPayload struct {
	Replace     bool                        `json:"replace"`
	SourceStamp json.RawMessage             `json:"source_stamp"`
	ID          []string                    `json:"id"`
	Dimension   map[string]dimensionPayload `json:"dimension"`
	Value       json.RawMessage             `json:"value"`
	Text        json.RawMessage             `json:"text"`
	Status      json.RawMessage             `json:"status"`
}

type selectionPayload struct {
	ID        []string                    `json:"id"`
	Dimension map[string]dimensionPayload `json:"dimension"`
}

type requestDimension struct {
	code       Code
	categories []Code
}

type channelRepresentation byte

const (
	representationDense channelRepresentation = iota + 1
	representationSparse
)

func ParseReplacement(providerSpelling, datasetSpelling string, data []byte, maxCells int64) (Replacement, error) {
	provider, err := NormalizeCode(providerSpelling)
	if err != nil {
		return Replacement{}, fmt.Errorf("provider code: %w", err)
	}
	dataset, err := NormalizeCode(datasetSpelling)
	if err != nil {
		return Replacement{}, fmt.Errorf("dataset code: %w", err)
	}

	var payload replacementPayload
	if err := jsonx.DecodeStrict(data, &payload); err != nil {
		return Replacement{}, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if payload.SourceStamp == nil {
		return Replacement{}, fmt.Errorf("source_stamp is required")
	}
	if payload.Value == nil {
		return Replacement{}, fmt.Errorf("value is required")
	}
	dimensions, cellCount, err := parseRequestDimensions(payload.ID, payload.Dimension, maxCells)
	if err != nil {
		return Replacement{}, err
	}

	numeric, representation, err := parseNumericChannel(payload.Value, cellCount)
	if err != nil {
		return Replacement{}, fmt.Errorf("value: %w", err)
	}
	textValues, err := parseStringChannel(payload.Text, cellCount, representation, false)
	if err != nil {
		return Replacement{}, fmt.Errorf("text: %w", err)
	}
	statuses, scalarStatus, err := parseStatusChannel(payload.Status, cellCount, representation)
	if err != nil {
		return Replacement{}, fmt.Errorf("status: %w", err)
	}

	requestCells := make(map[int64]Cell, len(numeric)+len(textValues)+len(statuses))
	for index, value := range numeric {
		v := value
		cell := requestCells[index]
		cell.Index = index
		cell.Numeric = &v
		requestCells[index] = cell
	}
	for index, value := range textValues {
		if cell := requestCells[index]; cell.Numeric != nil {
			return Replacement{}, fmt.Errorf("numeric and text values collide at index %d", index)
		}
		v := value
		cell := requestCells[index]
		cell.Index = index
		cell.Text = &v
		requestCells[index] = cell
	}
	for index, value := range statuses {
		v := value
		cell := requestCells[index]
		cell.Index = index
		cell.Status = &v
		requestCells[index] = cell
	}
	if scalarStatus != nil {
		for index := int64(0); index < cellCount; index++ {
			v := *scalarStatus
			cell := requestCells[index]
			cell.Index = index
			cell.Status = &v
			requestCells[index] = cell
		}
	}

	storedDimensions, cells, err := remapReplacement(dimensions, requestCells, cellCount)
	if err != nil {
		return Replacement{}, err
	}
	var valued int64
	for i := range cells {
		if cells[i].Numeric != nil || cells[i].Text != nil {
			valued++
		}
	}
	stamp := append(json.RawMessage(nil), payload.SourceStamp...)
	return Replacement{
		Provider: provider, Dataset: dataset, Replace: payload.Replace,
		SourceStamp: stamp, CellCount: cellCount, ValuedCount: valued,
		Dimensions: storedDimensions, Cells: cells,
	}, nil
}

func ParseSelection(data []byte) (Selection, error) {
	var payload selectionPayload
	if err := jsonx.DecodeStrict(data, &payload); err != nil {
		return Selection{}, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	dimensions, _, err := parseRequestDimensions(payload.ID, payload.Dimension, math.MaxInt64)
	if err != nil {
		return Selection{}, err
	}
	selection := Selection{Dimensions: make([]SelectionDimension, len(dimensions))}
	for i, dimension := range dimensions {
		selection.Dimensions[i] = SelectionDimension{Code: dimension.code, Categories: dimension.categories}
	}
	return selection, nil
}

func parseRequestDimensions(ids []string, payload map[string]dimensionPayload, limit int64) ([]requestDimension, int64, error) {
	if len(ids) < 1 || len(ids) > MaxDimensions {
		return nil, 0, fmt.Errorf("structure must contain between 1 and %d dimensions", MaxDimensions)
	}
	byKey := make(map[string]struct {
		code    Code
		payload dimensionPayload
	}, len(payload))
	for spelling, dimension := range payload {
		code, err := NormalizeCode(spelling)
		if err != nil {
			return nil, 0, fmt.Errorf("dimension code: %w", err)
		}
		if _, exists := byKey[code.Key]; exists {
			return nil, 0, fmt.Errorf("duplicate normalized dimension code %q", spelling)
		}
		byKey[code.Key] = struct {
			code    Code
			payload dimensionPayload
		}{code: code, payload: dimension}
	}
	if len(byKey) != len(ids) {
		return nil, 0, fmt.Errorf("id and dimension must contain the same dimensions")
	}

	dimensions := make([]requestDimension, len(ids))
	sizes := make([]int, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for position, spelling := range ids {
		idCode, err := NormalizeCode(spelling)
		if err != nil {
			return nil, 0, fmt.Errorf("id dimension code: %w", err)
		}
		if _, exists := seen[idCode.Key]; exists {
			return nil, 0, fmt.Errorf("duplicate normalized id dimension %q", spelling)
		}
		seen[idCode.Key] = struct{}{}
		entry, exists := byKey[idCode.Key]
		if !exists {
			return nil, 0, fmt.Errorf("id dimension %q is missing from dimension", spelling)
		}
		categories, err := parseCategories(entry.payload.Index)
		if err != nil {
			return nil, 0, fmt.Errorf("dimension %q: %w", spelling, err)
		}
		dimensions[position] = requestDimension{code: entry.code, categories: categories}
		sizes[position] = len(categories)
	}
	count, err := CheckedProduct(sizes, limit)
	if err != nil {
		return nil, 0, err
	}
	return dimensions, count, nil
}

func parseCategories(index map[string]int) ([]Code, error) {
	if len(index) == 0 {
		return nil, fmt.Errorf("at least one category is required")
	}
	categories := make([]Code, len(index))
	occupied := make([]bool, len(index))
	keys := make(map[string]struct{}, len(index))
	for spelling, position := range index {
		code, err := NormalizeCode(spelling)
		if err != nil {
			return nil, fmt.Errorf("category code: %w", err)
		}
		if _, exists := keys[code.Key]; exists {
			return nil, fmt.Errorf("duplicate normalized category code %q", spelling)
		}
		keys[code.Key] = struct{}{}
		if position < 0 || position >= len(index) || occupied[position] {
			return nil, fmt.Errorf("category indexes must be unique, zero-based, and contiguous")
		}
		occupied[position] = true
		categories[position] = code
	}
	return categories, nil
}

func parseNumericChannel(raw json.RawMessage, count int64) (map[int64]float64, channelRepresentation, error) {
	raw = bytes.TrimSpace(raw)
	values := make(map[int64]float64)
	if len(raw) == 0 {
		return nil, 0, fmt.Errorf("channel is required")
	}
	switch raw[0] {
	case '[':
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, 0, err
		}
		if int64(len(entries)) != count {
			return nil, 0, fmt.Errorf("dense channel length must equal %d", count)
		}
		for i, entry := range entries {
			if isNull(entry) {
				continue
			}
			value, err := parseFiniteFloat(entry)
			if err != nil {
				return nil, 0, fmt.Errorf("index %d: %w", i, err)
			}
			values[int64(i)] = value
		}
		return values, representationDense, nil
	case '{':
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, 0, err
		}
		for key, entry := range entries {
			index, err := parseSparseIndex(key, count)
			if err != nil {
				return nil, 0, err
			}
			if isNull(entry) {
				return nil, 0, fmt.Errorf("sparse channel index %s contains explicit null", key)
			}
			value, err := parseFiniteFloat(entry)
			if err != nil {
				return nil, 0, fmt.Errorf("index %s: %w", key, err)
			}
			values[index] = value
		}
		return values, representationSparse, nil
	default:
		return nil, 0, fmt.Errorf("channel must be an array or object")
	}
}

func parseStringChannel(raw json.RawMessage, count int64, representation channelRepresentation, required bool) (map[int64]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		if required {
			return nil, fmt.Errorf("channel is required")
		}
		return nil, nil
	}
	values := make(map[int64]string)
	if representation == representationDense {
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("channel must use the dense representation: %w", err)
		}
		if int64(len(entries)) != count {
			return nil, fmt.Errorf("dense channel length must equal %d", count)
		}
		for i, entry := range entries {
			if isNull(entry) {
				continue
			}
			var value string
			if err := json.Unmarshal(entry, &value); err != nil {
				return nil, fmt.Errorf("index %d must be a string or null", i)
			}
			values[int64(i)] = value
		}
		return values, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("channel must use the sparse representation: %w", err)
	}
	for key, entry := range entries {
		index, err := parseSparseIndex(key, count)
		if err != nil {
			return nil, err
		}
		if isNull(entry) {
			return nil, fmt.Errorf("sparse channel index %s contains explicit null", key)
		}
		var value string
		if err := json.Unmarshal(entry, &value); err != nil {
			return nil, fmt.Errorf("index %s must be a string", key)
		}
		values[index] = value
	}
	return values, nil
}

func parseStatusChannel(raw json.RawMessage, count int64, representation channelRepresentation) (map[int64]string, *string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, nil
	}
	if trimmed[0] == '"' {
		var scalar string
		if err := json.Unmarshal(trimmed, &scalar); err != nil {
			return nil, nil, err
		}
		return nil, &scalar, nil
	}
	values, err := parseStringChannel(trimmed, count, representation, true)
	return values, nil, err
}

func parseSparseIndex(key string, count int64) (int64, error) {
	if key == "" || (len(key) > 1 && key[0] == '0') || key[0] == '-' {
		return 0, fmt.Errorf("sparse index %q is not canonical", key)
	}
	index, err := strconv.ParseInt(key, 10, 64)
	if err != nil || index < 0 || index >= count {
		return 0, fmt.Errorf("sparse index %q is outside the cube", key)
	}
	return index, nil
}

func parseFiniteFloat(raw json.RawMessage) (float64, error) {
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return 0, fmt.Errorf("must be a JSON number")
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("must fit finite float64")
	}
	return value, nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func remapReplacement(dimensions []requestDimension, requestCells map[int64]Cell, cellCount int64) ([]Dimension, []Cell, error) {
	stored := make([]Dimension, len(dimensions))
	for i, dimension := range dimensions {
		categories := append([]Code(nil), dimension.categories...)
		sort.Slice(categories, func(a, b int) bool { return categories[a].Key < categories[b].Key })
		stored[i] = Dimension{Code: dimension.code, Categories: make([]Category, len(categories))}
		for j, category := range categories {
			stored[i].Categories[j] = Category{Code: category, Position: j}
		}
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].Code.Key < stored[j].Code.Key })
	for i := range stored {
		stored[i].Position = i
	}

	requestSizes := make([]int, len(dimensions))
	storedSizes := make([]int, len(stored))
	dimensionPosition := make(map[string]int, len(stored))
	categoryPositions := make(map[string]map[string]int, len(stored))
	for i, dimension := range dimensions {
		requestSizes[i] = len(dimension.categories)
	}
	for i, dimension := range stored {
		storedSizes[i] = len(dimension.Categories)
		dimensionPosition[dimension.Code.Key] = i
		positions := make(map[string]int, len(dimension.Categories))
		for _, category := range dimension.Categories {
			positions[category.Code.Key] = category.Position
		}
		categoryPositions[dimension.Code.Key] = positions
	}
	requestStrides := Strides(requestSizes)
	storedStrides := Strides(storedSizes)
	cells := make([]Cell, 0, len(requestCells))
	for requestIndex, cell := range requestCells {
		if requestIndex < 0 || requestIndex >= cellCount {
			return nil, nil, fmt.Errorf("cell index %d is outside the cube", requestIndex)
		}
		requestCoordinates, err := Coordinates(requestIndex, requestSizes, requestStrides)
		if err != nil {
			return nil, nil, err
		}
		storedCoordinates := make([]int, len(stored))
		for requestDimensionPosition, coordinate := range requestCoordinates {
			dimension := dimensions[requestDimensionPosition]
			storedDimensionPosition := dimensionPosition[dimension.code.Key]
			category := dimension.categories[coordinate]
			storedCoordinates[storedDimensionPosition] = categoryPositions[dimension.code.Key][category.Key]
		}
		storedIndex, err := CellIndex(storedCoordinates, storedSizes, storedStrides)
		if err != nil {
			return nil, nil, err
		}
		cell.Index = storedIndex
		cells = append(cells, cell)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].Index < cells[j].Index })
	return stored, cells, nil
}
