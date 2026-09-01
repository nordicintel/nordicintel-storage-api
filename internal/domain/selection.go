package domain

import "fmt"

func ResolveSelection(selection Selection, stored []Dimension, maxCells int64) ([]Dimension, []int64, error) {
	if len(selection.Dimensions) != len(stored) {
		return nil, nil, fmt.Errorf("query must include every dataset dimension exactly once")
	}
	storedByKey := make(map[string]Dimension, len(stored))
	storedSizes := make([]int, len(stored))
	for _, dimension := range stored {
		storedByKey[dimension.Code.Key] = dimension
		storedSizes[dimension.Position] = len(dimension.Categories)
	}
	storedStrides := Strides(storedSizes)

	output := make([]Dimension, len(selection.Dimensions))
	selectedStoredPositions := make([][]int, len(selection.Dimensions))
	seenDimensions := make(map[string]struct{}, len(selection.Dimensions))
	outputSizes := make([]int, len(selection.Dimensions))
	for i, requested := range selection.Dimensions {
		if _, duplicate := seenDimensions[requested.Code.Key]; duplicate {
			return nil, nil, fmt.Errorf("query contains a duplicate dimension")
		}
		seenDimensions[requested.Code.Key] = struct{}{}
		storedDimension, exists := storedByKey[requested.Code.Key]
		if !exists {
			return nil, nil, fmt.Errorf("query contains an unknown dimension")
		}
		categoryByKey := make(map[string]Category, len(storedDimension.Categories))
		for _, category := range storedDimension.Categories {
			categoryByKey[category.Code.Key] = category
		}
		output[i] = Dimension{Code: storedDimension.Code, Position: i, Categories: make([]Category, len(requested.Categories))}
		selectedStoredPositions[i] = make([]int, len(requested.Categories))
		seenCategories := make(map[string]struct{}, len(requested.Categories))
		for j, requestedCategory := range requested.Categories {
			if _, duplicate := seenCategories[requestedCategory.Key]; duplicate {
				return nil, nil, fmt.Errorf("query contains a duplicate category")
			}
			seenCategories[requestedCategory.Key] = struct{}{}
			storedCategory, exists := categoryByKey[requestedCategory.Key]
			if !exists {
				return nil, nil, fmt.Errorf("query contains an unknown category")
			}
			output[i].Categories[j] = Category{Code: storedCategory.Code, Position: j}
			selectedStoredPositions[i][j] = storedCategory.Position
		}
		outputSizes[i] = len(requested.Categories)
	}

	count, err := CheckedProduct(outputSizes, maxCells)
	if err != nil {
		return nil, nil, err
	}
	outputStrides := Strides(outputSizes)
	indices := make([]int64, count)
	for outputIndex := int64(0); outputIndex < count; outputIndex++ {
		coords, err := Coordinates(outputIndex, outputSizes, outputStrides)
		if err != nil {
			return nil, nil, err
		}
		storedCoordinates := make([]int, len(stored))
		for outputDimensionPosition, outputCategoryPosition := range coords {
			storedDimension := storedByKey[selection.Dimensions[outputDimensionPosition].Code.Key]
			storedCoordinates[storedDimension.Position] = selectedStoredPositions[outputDimensionPosition][outputCategoryPosition]
		}
		index, err := CellIndex(storedCoordinates, storedSizes, storedStrides)
		if err != nil {
			return nil, nil, err
		}
		indices[outputIndex] = index
	}
	return output, indices, nil
}
