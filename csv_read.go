package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// csvReader reads CSV the way Ruby's CSV does with liberal_parsing: true,
// which is what the Rails app used. The differences from encoding/csv that
// matter here: a blank line is a record with no fields, text after a closing
// quote is kept along with the quotes, and an unquoted empty field is told
// apart from a quoted one.
//
// One deliberate difference from Ruby: \r\n, \n and \r all end a row, in any
// mix. Ruby used the first line ending in the file and rejected any other, so
// every file Ruby accepted reads the same here.
type csvReader struct {
	r    *bufio.Reader
	line int // records read so far; Ruby's lineno
	done bool
}

type csvField struct {
	value string
	null  bool // unquoted and empty; Ruby returns nil for these
}

type csvParseError struct {
	msg  string
	line int
}

func (e *csvParseError) Error() string { return fmt.Sprintf("%s in line %d.", e.msg, e.line) }

func newCSVReader(r io.Reader) *csvReader {
	return &csvReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Read returns the next record, or io.EOF.
func (c *csvReader) Read() ([]csvField, error) {
	if c.done {
		return nil, io.EOF
	}
	var row []csvField
	for {
		f, err := c.field()
		if err != nil {
			return nil, err
		}
		b, err := c.r.Peek(1)
		switch {
		case err != nil && !errors.Is(err, io.EOF):
			return nil, err
		case err != nil:
			c.done = true
			if len(row) == 0 && f.null {
				return nil, io.EOF
			}
			c.line++
			return append(row, f), nil
		case b[0] == ',':
			c.r.Discard(1)
			row = append(row, f)
		default:
			c.skipLineEnd()
			c.line++
			if len(row) == 0 && f.null {
				return []csvField{}, nil
			}
			return append(row, f), nil
		}
	}
}

// field reads one field and stops before the comma or line ending after it.
func (c *csvReader) field() (csvField, error) {
	if b, _ := c.r.Peek(1); len(b) == 0 || b[0] != '"' {
		s, err := c.unquotedRun()
		return csvField{value: s, null: s == ""}, err
	}

	var v strings.Builder
	n, err := c.quoteRun()
	if err != nil {
		return csvField{}, err
	}
	if n%2 == 0 {
		v.WriteString(strings.Repeat(`"`, (n-2)/2))
	} else {
		v.WriteString(strings.Repeat(`"`, n/2))
		for {
			s, err := c.runUntil(func(b byte) bool { return b == '"' })
			if err != nil {
				return csvField{}, err
			}
			v.WriteString(s)
			m, err := c.quoteRun()
			if err != nil {
				return csvField{}, err
			}
			if m == 0 {
				c.done = true
				c.line++
				return csvField{}, &csvParseError{"Unclosed quoted field", c.line}
			}
			if m == 1 {
				break
			}
			v.WriteString(strings.Repeat(`"`, m/2))
			if m%2 == 1 {
				break
			}
		}
	}
	rest, err := c.unquotedRun()
	if rest != "" {
		return csvField{value: `"` + v.String() + `"` + rest}, err
	}
	return csvField{value: v.String()}, err
}

func (c *csvReader) unquotedRun() (string, error) {
	return c.runUntil(func(b byte) bool { return b == ',' || b == '\r' || b == '\n' })
}

// runUntil reads bytes up to, not including, the first one that stop accepts.
// The delimiters are all ASCII, so working on bytes is safe for UTF-8.
func (c *csvReader) runUntil(stop func(byte) bool) (string, error) {
	var s []byte
	for {
		b, err := c.r.ReadByte()
		if errors.Is(err, io.EOF) {
			return string(s), nil
		}
		if err != nil {
			return "", err
		}
		if stop(b) {
			c.r.UnreadByte()
			return string(s), nil
		}
		s = append(s, b)
	}
}

func (c *csvReader) quoteRun() (int, error) {
	s, err := c.runUntil(func(b byte) bool { return b != '"' })
	return len(s), err
}

// skipLineEnd consumes \r\n, \n or \r, preferring \r\n.
func (c *csvReader) skipLineEnd() {
	if b, _ := c.r.Peek(2); string(b) == "\r\n" {
		c.r.Discard(2)
		return
	}
	c.r.Discard(1)
}
