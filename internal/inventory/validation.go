package inventory

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

// invalidInput is kept separate from database errors so handlers can return
// INVALID_ARGUMENT without exposing SQL or connection details.
type invalidInput struct{ message string }

func (e *invalidInput) Error() string { return e.message }

func normalizeID(value, field string) (string, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", &invalidInput{message: field + " must be a UUID"}
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(decoded) != 16 {
		return "", &invalidInput{message: field + " must be a UUID"}
	}
	return strings.ToLower(value), nil
}

// normalizeItems merges repeated products before any stock is touched. Sorting
// makes Reserve and Release take product locks in the same order.
func normalizeItems(items []Item) ([]Item, error) {
	if len(items) == 0 {
		return nil, &invalidInput{message: "items must contain at least one product"}
	}
	quantities := make(map[string]int32, len(items))
	for i, item := range items {
		id, err := normalizeID(item.ProductID, fmt.Sprintf("items[%d].product_id", i))
		if err != nil {
			return nil, err
		}
		if item.Quantity <= 0 {
			return nil, &invalidInput{message: fmt.Sprintf("items[%d].quantity must be greater than zero", i)}
		}
		if item.Quantity > math.MaxInt32-quantities[id] {
			return nil, &invalidInput{message: "total quantity for a product exceeds int32"}
		}
		quantities[id] += item.Quantity
	}
	merged := make([]Item, 0, len(quantities))
	for id, quantity := range quantities {
		merged = append(merged, Item{ProductID: id, Quantity: quantity})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ProductID < merged[j].ProductID })
	return merged, nil
}
