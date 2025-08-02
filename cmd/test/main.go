package main

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

type FloatPayload struct {
	Value float64 `json:"value"`
}

type DecimalPayload struct {
	Value decimal.Decimal `json:"value"`
}

type AnyPayload struct {
	Value any `json:"value"`
}

func main() {
	data := []byte(`{"value": 0.1}`)

	// Using float64
	var f FloatPayload
	json.Unmarshal(data, &f)
	// Print with 18 decimal places to show inaccuracy
	fmt.Printf("float64 value: %.18f\n", f.Value)

	// Using shopspring/decimal
	var d DecimalPayload
	json.Unmarshal(data, &d)
	// Print using String() which is exact
	fmt.Printf("decimal value: %s\n", d.Value.String())

	var a AnyPayload
	json.Unmarshal(data, &a)

	decimalValue, ok := a.Value.(decimal.Decimal)
	if !ok {
		fmt.Println("Value is not a decimal.Decimal")
		return
	}

	fmt.Printf("any value: %.18f\n", decimalValue.Add(decimal.NewFromFloat(0.2)).String())
}
