// Package apispec generates the OpenAPI 3.1 contract from the Go request/response types and an
// operation table. `go run ./cmd/openapi-gen` writes api/openapi.yaml; tests fail when the
// committed file drifts or when a mounted route is undocumented.
package apispec

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

// registry builds component schemas by reflection.
type registry struct {
	schemas map[string]any
	names   map[reflect.Type]string
}

func newRegistry() *registry {
	return &registry{schemas: map[string]any{}, names: map[reflect.Type]string{}}
}

var (
	timeType = reflect.TypeOf(time.Time{})
	uuidType = reflect.TypeOf(uuid.UUID{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

func componentName(t reflect.Type) string {
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	if pkg == "apispec" {
		return t.Name()
	}
	return pkg + "." + t.Name()
}

// schemaOf returns a schema for t, registering named struct types as components.
func (r *registry) schemaOf(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case uuidType:
		return map[string]any{"type": "string", "format": "uuid"}
	case rawType:
		return map[string]any{"description": "Arbitrary JSON value."}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if t.Kind() == reflect.Int64 || t.Kind() == reflect.Uint64 {
			return map[string]any{"type": "integer", "format": "int64"}
		}
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			return map[string]any{"type": "string", "format": "byte"}
		}
		return map[string]any{"type": "array", "items": r.schemaOf(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": r.schemaOf(t.Elem())}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		if t.Name() == "" {
			return r.structSchema(t)
		}
		name, ok := r.names[t]
		if !ok {
			name = componentName(t)
			r.names[t] = name
			r.schemas[name] = map[string]any{} // placeholder for recursion
			r.schemas[name] = r.structSchema(t)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{}
}

func (r *registry) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	r.collect(t, props, &required)
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func (r *registry) collect(t reflect.Type, props map[string]any, required *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				r.collect(ft, props, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = r.schemaOf(f.Type)
		if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
			*required = append(*required, name)
		}
	}
}
