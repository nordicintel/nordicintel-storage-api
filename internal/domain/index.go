package domain

import (
	"fmt"
	"math"
)

type CellLimitError struct{ Limit int64 }

func (e CellLimitError) Error() string { return fmt.Sprintf("logical cell count exceeds %d", e.Limit) }

func CheckedProduct(sizes []int, limit int64) (int64, error) {
	if len(sizes) == 0 {
		return 0, fmt.Errorf("at least one dimension is required")
	}
	product := int64(1)
	for _, size := range sizes {
		if size < 1 {
			return 0, fmt.Errorf("every dimension requires at least one category")
		}
		if product > math.MaxInt64/int64(size) {
			return 0, fmt.Errorf("logical cell count overflows int64")
		}
		product *= int64(size)
		if product > limit {
			return 0, CellLimitError{Limit: limit}
		}
	}
	return product, nil
}

func Strides(sizes []int) []int64 {
	strides := make([]int64, len(sizes))
	stride := int64(1)
	for i := len(sizes) - 1; i >= 0; i-- {
		strides[i] = stride
		stride *= int64(sizes[i])
	}
	return strides
}

func Coordinates(index int64, sizes []int, strides []int64) ([]int, error) {
	if len(sizes) != len(strides) {
		return nil, fmt.Errorf("size and stride lengths differ")
	}
	coords := make([]int, len(sizes))
	remaining := index
	for i := range sizes {
		if remaining < 0 || strides[i] < 1 {
			return nil, fmt.Errorf("index is outside the cube")
		}
		coords[i] = int(remaining / strides[i])
		if coords[i] >= sizes[i] {
			return nil, fmt.Errorf("index is outside the cube")
		}
		remaining %= strides[i]
	}
	return coords, nil
}

func CellIndex(coords []int, sizes []int, strides []int64) (int64, error) {
	if len(coords) != len(sizes) || len(sizes) != len(strides) {
		return 0, fmt.Errorf("coordinate, size, and stride lengths differ")
	}
	var index int64
	for i, coordinate := range coords {
		if coordinate < 0 || coordinate >= sizes[i] {
			return 0, fmt.Errorf("coordinate is outside the cube")
		}
		index += int64(coordinate) * strides[i]
	}
	return index, nil
}
