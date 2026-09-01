package jsonx

import (
	"encoding/json"
	"strings"
	"testing"
)

type target struct {
	Name   string          `json:"name"`
	Nested json.RawMessage `json:"nested"`
	Values []int           `json:"values"`
}

func TestDecodeStrictAcceptsWellFormedDocuments(t *testing.T) {
	var got target
	if err := DecodeStrict([]byte(`{"name":"a","nested":{"x":[1,{"y":null}]},"values":[1,2]}`), &got); err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if got.Name != "a" || len(got.Values) != 2 {
		t.Fatalf("decoded = %+v", got)
	}
	if string(got.Nested) != `{"x":[1,{"y":null}]}` {
		t.Fatalf("nested = %s", got.Nested)
	}
}

func TestDecodeStrictRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"top level", `{"name":"a","name":"b"}`},
		{"inside a nested object", `{"nested":{"x":1,"x":2}}`},
		{"deeply nested", `{"nested":{"a":{"b":[{"c":1,"c":2}]}}}`},
		{"inside an array element", `{"nested":[{"d":1,"d":2}]}`},
		{"duplicate of a known field after an unknown-free prefix", `{"values":[1],"values":[2]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got target
			err := DecodeStrict([]byte(tc.body), &got)
			if err == nil {
				t.Fatal("duplicate key accepted")
			}
			if !strings.Contains(err.Error(), "duplicate JSON object key") {
				t.Fatalf("error = %v, want a duplicate-key error", err)
			}
		})
	}
}

func TestDecodeStrictRejectsUnknownFields(t *testing.T) {
	var got target
	if err := DecodeStrict([]byte(`{"name":"a","surprise":1}`), &got); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDecodeStrictRejectsTrailingContent(t *testing.T) {
	for _, body := range []string{
		`{"name":"a"} 7`,
		`{"name":"a"}{"name":"b"}`,
		`{"name":"a"}null`,
		`{"name":"a"}[]`,
	} {
		var got target
		err := DecodeStrict([]byte(body), &got)
		if err == nil {
			t.Fatalf("body %s accepted", body)
		}
		if !strings.Contains(err.Error(), "trailing JSON content") {
			t.Fatalf("body %s: error = %v, want a trailing-content error", body, err)
		}
	}
}

func TestDecodeStrictRejectsMalformedAndNonUTF8Documents(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"truncated object", []byte(`{"name":`)},
		{"unclosed array", []byte(`{"values":[1,2`)},
		{"empty body", []byte(``)},
		{"invalid utf-8", []byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got target
			if err := DecodeStrict(tc.body, &got); err == nil {
				t.Fatal("malformed document accepted")
			}
		})
	}
}

func TestDecodeStrictAllowsRepeatedKeysInSiblingObjects(t *testing.T) {
	var got target
	if err := DecodeStrict([]byte(`{"nested":{"a":{"x":1},"b":{"x":2}}}`), &got); err != nil {
		t.Fatalf("sibling objects may reuse a key: %v", err)
	}
}
