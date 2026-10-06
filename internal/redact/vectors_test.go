package redact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The server's parity corpus (a copy of pkg/redact/testdata/vectors.json in
// fixwire/fixwire, kept identical): this port must mask every string and
// document exactly as the server does.

type vectors struct {
	Detectors     []string            `json:"detectors"`
	SensitiveKeys []string            `json:"sensitive_keys"`
	Fixtures      map[string][]string `json:"fixtures"`
	Strings       []struct {
		Name     string   `json:"name"`
		Input    string   `json:"input"`
		Masked   string   `json:"masked"`
		Findings []string `json:"findings"`
	} `json:"strings"`
	Documents []struct {
		Name   string          `json:"name"`
		Input  json.RawMessage `json:"input"`
		Masked json.RawMessage `json:"masked"`
		Count  int             `json:"count"`
	} `json:"documents"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVectors(t *testing.T) {
	v := loadVectors(t)
	expand := func(s string) string {
		for name, parts := range v.Fixtures {
			s = strings.ReplaceAll(s, "{{"+name+"}}", strings.Join(parts, ""))
		}
		return s
	}
	if !slices.Equal(v.Detectors, DefaultDetectors) {
		t.Errorf("detectors = %v, the server's are %v", DefaultDetectors, v.Detectors)
	}
	if !slices.Equal(v.SensitiveKeys, DefaultSensitiveKeys) {
		t.Errorf("sensitive keys = %v, the server's are %v", DefaultSensitiveKeys, v.SensitiveKeys)
	}
	r := Default()
	for _, c := range v.Strings {
		masked, fs := r.Mask(expand(c.Input))
		names := []string{}
		for _, f := range fs {
			names = append(names, f.Detector)
		}
		if masked != c.Masked || !slices.Equal(names, c.Findings) {
			t.Errorf("%s: masked %q %v, want %q %v", c.Name, masked, names, c.Masked, c.Findings)
		}
	}
	for _, c := range v.Documents {
		got, n := r.Walk(decode(t, expand(string(c.Input))))
		if want := decode(t, string(c.Masked)); !reflect.DeepEqual(got, want) || n != c.Count {
			b, _ := json.Marshal(got)
			t.Errorf("%s: masked %s (%d), want %s (%d)", c.Name, b, n, c.Masked, c.Count)
		}
	}
}
