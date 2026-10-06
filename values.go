package fixwire

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// What a value sent (a context, an extra, an attribute, a breadcrumb's data)
// may hold, as every Fixwire SDK bounds it (sdks/PROTOCOL.md §13): maps and
// lists 10 deep and 100 wide, and 10,000 of them walked. A map or list past
// that is "[Object]" or "[Array]", one inside itself "[Circular ~]", one that
// can't be read (its MarshalJSON or Error panics) "[Unreadable]".
const (
	maxDepth   = 10
	maxBreadth = 100
	maxObjects = 10000

	circular   = "[Circular ~]"
	unreadable = "[Unreadable]"
)

// redactAhead is how far past MaxValueLength redaction reads a string, so
// that a secret the cut goes through (a PEM key, a JWT) is still found.
const redactAhead = 16 << 10

// plain is v in JSON's own types (maps, lists, strings, numbers, bools,
// nil), within the bounds above and with strings no longer than redaction
// reads: a copy the SDK may mask in place. Values go as encoding/json would
// write them, but errors as their message, and NaN and the infinities as
// the strings "NaN", "Infinity" and "-Infinity".
func (c *Client) plain(v any) any {
	w := walker{limit: c.opts.MaxValueLength + redactAhead, left: maxObjects}
	return w.walk(v, 0)
}

type walker struct {
	limit int       // the bytes a string keeps
	left  int       // the maps, lists and structs still to walk
	path  []uintptr // the maps, lists and pointers the value walked is in
}

func (w *walker) walk(v any, depth int) (out any) {
	defer func() {
		if recover() != nil {
			out = unreadable
		}
	}()
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return ahead(x, w.limit)
	case bool, json.Number:
		return x
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return finite(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if rv.IsNil() {
			return nil
		}
	}
	switch x := v.(type) {
	case json.Marshaler:
		b, err := x.MarshalJSON()
		if err != nil {
			return unreadable
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var decoded any
		if dec.Decode(&decoded) != nil {
			return unreadable
		}
		return w.walk(decoded, depth)
	case error:
		return ahead(x.Error(), w.limit)
	case encoding.TextMarshaler:
		b, err := x.MarshalText()
		if err != nil {
			return unreadable
		}
		return ahead(string(b), w.limit)
	}
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint()
	case reflect.Float32:
		if f := rv.Float(); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return json.Number(strconv.FormatFloat(f, 'g', -1, 32))
		}
		return finite(rv.Float())
	case reflect.Float64:
		return finite(rv.Float())
	case reflect.Complex64, reflect.Complex128:
		return strconv.FormatComplex(rv.Complex(), 'g', -1, 128)
	case reflect.String:
		return ahead(rv.String(), w.limit)
	case reflect.Pointer:
		if !w.push(rv.Pointer()) {
			return circular
		}
		defer w.pop()
		return w.walk(rv.Elem().Interface(), depth)
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		return w.container(rv, depth)
	}
	return unreadable // channels and functions
}

// container walks a map, a list or a struct.
func (w *walker) container(rv reflect.Value, depth int) any {
	marker := "[Object]"
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		marker = "[Array]"
	}
	if rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice {
		if !w.push(rv.Pointer()) {
			return circular
		}
		defer w.pop()
	}
	if depth >= maxDepth || w.left <= 0 {
		return marker
	}
	w.left--
	switch rv.Kind() {
	case reflect.Map:
		out := make(map[string]any, min(rv.Len(), maxBreadth))
		for it, n := rv.MapRange(), 0; n < maxBreadth && it.Next(); n++ {
			out[ahead(keyOf(it.Key()), w.limit)] = w.walk(it.Value().Interface(), depth+1)
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		w.fields(rv, out, depth, 0)
		return out
	}
	if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
		// Bytes, as encoding/json writes them: base64.
		b := rv.Bytes()
		return base64.StdEncoding.EncodeToString(b[:min(len(b), w.limit)])
	}
	out := make([]any, min(rv.Len(), maxBreadth))
	for i := range out {
		out[i] = w.walk(rv.Index(i).Interface(), depth+1)
	}
	return out
}

// fields adds a struct's exported fields to out, named as encoding/json
// names them; the fields of an embedded struct are promoted.
func (w *walker) fields(rv reflect.Value, out map[string]any, depth, embedded int) {
	t := rv.Type()
	for i := 0; i < t.NumField() && len(out) < maxBreadth; i++ {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" && opts == "" {
			continue
		}
		fv := rv.Field(i)
		if f.Anonymous && name == "" && embedded < maxDepth {
			if fv.Kind() == reflect.Pointer && !fv.IsNil() {
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				w.fields(fv, out, depth, embedded+1)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, taken := out[name]; taken || slices.Contains(strings.Split(opts, ","), "omitempty") && emptyValue(fv) {
			continue
		}
		out[name] = w.walk(fv.Interface(), depth+1)
	}
}

func (w *walker) push(p uintptr) bool {
	if slices.Contains(w.path, p) {
		return false
	}
	w.path = append(w.path, p)
	return true
}

func (w *walker) pop() { w.path = w.path[:len(w.path)-1] }

// keyOf is a map key as encoding/json writes it.
func keyOf(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	if tm, ok := k.Interface().(encoding.TextMarshaler); ok {
		if b, err := tm.MarshalText(); err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(k.Interface())
}

// emptyValue is encoding/json's omitempty test.
func emptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.Struct:
		return false
	}
	return v.IsZero()
}

// finite is f, or its name when JSON has no number for it.
func finite(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return f
}

// ahead is s as far as redaction reads it: the limit bytes kept and the
// redactAhead after, cut on a character boundary.
func ahead(s string, limit int) string {
	if len(s) <= limit+redactAhead {
		return s
	}
	return s[:boundary(s, limit+redactAhead)]
}

// cut is s in at most limit bytes: cut on a character boundary and ending
// in "..." when it was longer.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:boundary(s, max(limit-len("..."), 0))] + "..."
}

// boundary is n, or the start of the character at n when n falls inside
// one.
func boundary(s string, n int) int {
	for i := 0; i < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(s[n]); i++ {
		n--
	}
	return n
}

// eachString replaces the strings in v (JSON's own types) and its maps' keys
// with f's, in place where it can.
func eachString(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for i := range x {
			x[i] = eachString(x[i], f)
		}
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = f(s)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, s := range x {
			out[f(k)] = f(s)
		}
		return out
	case map[string]any:
		var renamed []string
		for k, e := range x {
			x[k] = eachString(e, f)
			if f(k) != k {
				renamed = append(renamed, k)
			}
		}
		for _, k := range renamed {
			x[f(k)] = x[k]
			delete(x, k)
		}
	}
	return v
}
