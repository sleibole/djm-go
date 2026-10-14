package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

// testdata/csvparse.json holds random inputs and what Ruby's CSV made of them;
// testdata/golden.rb regenerates it.
func TestCSVReaderMatchesRuby(t *testing.T) {
	data, err := os.ReadFile("testdata/csvparse.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input   string      `json:"input"`
		Records [][]*string `json:"records"`
		Error   *string     `json:"error"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}

	failures, mixed := 0, 0
	for _, c := range cases {
		records, gotErr := readAllCSV(c.Input)
		if c.Error != nil && isLineEndingError(*c.Error) {
			// Ruby rejected a line ending that differs from the first one; we
			// accept any mix, so the only error left is a quote opened later.
			mixed++
			if gotErr != "" && !strings.HasPrefix(gotErr, "Unclosed quoted field") {
				t.Errorf("input %q: got err=%q, want mixed line endings accepted", c.Input, gotErr)
			}
			continue
		}
		want, wantErr := c.Records, ""
		if c.Error != nil {
			// Only the error is compared: the importer stops at it.
			records, want, wantErr = nil, nil, *c.Error
		}
		if len(want) == 0 {
			want = nil
		}
		if gotErr != wantErr || !reflect.DeepEqual(records, want) {
			failures++
			if failures <= 15 {
				t.Errorf("input %q:\n got  %s err=%q\n want %s err=%q", c.Input, show(records), gotErr, show(want), wantErr)
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d cases differ from Ruby", failures, len(cases)-mixed)
	}
	if mixed == 0 {
		t.Error("no mixed line ending cases")
	}
}

func isLineEndingError(msg string) bool {
	for _, s := range []string{"Illegal end-of-line sequence", "Unquoted fields do not allow new line", "New line must be"} {
		if strings.HasPrefix(msg, s) {
			return true
		}
	}
	return false
}

func TestCSVReaderMixedLineEndings(t *testing.T) {
	got, err := readAllCSV("title,artist\nA,B\r\nC,D\rE,\"F\nG\"\r\n\nH,I")
	s := func(v string) *string { return &v }
	want := [][]*string{{s("title"), s("artist")}, {s("A"), s("B")}, {s("C"), s("D")}, {s("E"), s("F\nG")}, {}, {s("H"), s("I")}}
	if err != "" || !reflect.DeepEqual(got, want) {
		t.Errorf("got %s err=%q, want %s", show(got), err, show(want))
	}
}

func readAllCSV(input string) ([][]*string, string) {
	r := newCSVReader(strings.NewReader(input))
	var out [][]*string
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, ""
		}
		if err != nil {
			return out, err.Error()
		}
		row := []*string{}
		for _, f := range rec {
			if f.null {
				row = append(row, nil)
			} else {
				row = append(row, &f.value)
			}
		}
		out = append(out, row)
	}
}

func show(records [][]*string) string {
	var b strings.Builder
	for _, r := range records {
		b.WriteString("[")
		for i, f := range r {
			if i > 0 {
				b.WriteString(" ")
			}
			if f == nil {
				b.WriteString("nil")
			} else {
				b.WriteString(`"` + strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(*f) + `"`)
			}
		}
		b.WriteString("]")
	}
	return b.String()
}
