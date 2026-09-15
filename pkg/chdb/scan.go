package chdb

import (
	"reflect"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// newPtrFor returns a typed pointer suitable for driver.Rows.Scan based on
// the column's ScanType. clickhouse-go requires concrete typed pointers —
// `*interface{}` only works for a subset of types.
func newPtrFor(ct driver.ColumnType) any {
	t := ct.ScanType()
	if t == nil {
		var v any
		return &v
	}
	return reflect.New(t).Interface()
}

// derefAny unwraps the pointer returned by newPtrFor to its underlying value.
func derefAny(p any) any {
	if p == nil {
		return nil
	}
	v := reflect.ValueOf(p)
	if v.Kind() != reflect.Pointer {
		return p
	}
	if v.IsNil() {
		return nil
	}
	return v.Elem().Interface()
}
