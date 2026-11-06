package raster

import (
	"bytes"
	"encoding/json"
	"math"
)

// arguments reads the call's JSON object the way SWArguments.m reads it: a value of the
// wrong type counts as absent, and true and false count as the numbers 1 and 0.
type arguments map[string]json.RawMessage

func parseArguments(data json.RawMessage) (arguments, error) {
	var values map[string]json.RawMessage
	if json.Unmarshal(data, &values) != nil || values == nil {
		return nil, failure(NameType, "graphics: the arguments are not a JSON object")
	}
	return values, nil
}

func (a arguments) has(key string) bool {
	_, present := a[key]
	return present
}

func numberOf(value json.RawMessage) (float64, bool) {
	value = bytes.TrimSpace(value)
	switch string(value) {
	case "true":
		return 1, true
	case "false":
		return 0, true
	}
	var number float64
	if len(value) == 0 || (value[0] != '-' && (value[0] < '0' || value[0] > '9')) || json.Unmarshal(value, &number) != nil {
		return 0, false
	}
	return number, true
}

func integerOf(number float64) int {
	return int(math.Trunc(number))
}

func (a arguments) number(key string) (float64, bool) {
	return numberOf(a[key])
}

func (a arguments) float(key string, fallback float64) float64 {
	if number, ok := a.number(key); ok {
		return number
	}
	return fallback
}

func (a arguments) integer(key string, fallback int) int {
	if number, ok := a.number(key); ok {
		return integerOf(number)
	}
	return fallback
}

func (a arguments) string(key string) (string, bool) {
	var value string
	raw := bytes.TrimSpace(a[key])
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func (a arguments) array(key string) ([]json.RawMessage, bool) {
	var items []json.RawMessage
	raw := bytes.TrimSpace(a[key])
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	return items, true
}

func numbersOf(items []json.RawMessage) ([]float64, bool) {
	values := make([]float64, len(items))
	for index, item := range items {
		number, ok := numberOf(item)
		if !ok {
			return nil, false
		}
		values[index] = number
	}
	return values, true
}

func (a arguments) numbers(key string) ([]float64, bool) {
	items, ok := a.array(key)
	if !ok {
		return nil, false
	}
	return numbersOf(items)
}

func (a arguments) rect(key string) (Rect, bool) {
	values, ok := a.numbers(key)
	if !ok || len(values) != 4 {
		return Rect{}, false
	}
	return Rect{values[0], values[1], values[2], values[3]}, true
}

func (a arguments) region() *Rect {
	if region, ok := a.rect("region"); ok {
		return &region
	}
	return nil
}

func (a arguments) point(key string) (Point, bool) {
	values, ok := a.numbers(key)
	if !ok || len(values) != 2 {
		return Point{}, false
	}
	return Point{values[0], values[1]}, true
}

// objects answers the array only when every item is an object, and no items otherwise.
func (a arguments) objects(key string) []arguments {
	items, ok := a.array(key)
	if !ok {
		return nil
	}
	values := make([]arguments, len(items))
	for index, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 || item[0] != '{' || json.Unmarshal(item, &values[index]) != nil {
			return nil
		}
	}
	return values
}

// timeout is timeoutMs, or Infinity when it is absent or null.
func (a arguments) timeout() float64 {
	return a.float("timeoutMs", math.Inf(1))
}
