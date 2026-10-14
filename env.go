package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// loadDotEnv reads a local .env file into the process environment.
// A missing file is not an error. Variables already set are left alone,
// so production config and the shell win over the file.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return applyDotEnv(f)
}

func applyDotEnv(r io.Reader) error {
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			return fmt.Errorf(".env:%d: expected KEY=VALUE", lineNo)
		}
		val = unquoteEnvValue(strings.TrimSpace(val))
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return sc.Err()
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func unquoteEnvValue(val string) string {
	if len(val) >= 2 {
		if q := val[0]; (q == '"' || q == '\'') && val[len(val)-1] == q {
			return val[1 : len(val)-1]
		}
	}
	if i := strings.Index(val, " #"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	return val
}
