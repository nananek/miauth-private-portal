package config

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var envKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ParseEnvFile parses a minimal KEY=VALUE config file: blank lines and
// lines starting with '#' are ignored, KEY must match ^[A-Z][A-Z0-9_]*$,
// VALUE may be wrapped in matching single or double quotes, and a
// duplicate key or any other malformed line is a parse error naming the
// file and the 1-based line number. Error text never includes the
// offending value, so it is always safe to log or return to a caller.
func ParseEnvFile(r io.Reader) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}

		key := strings.TrimSpace(line[:idx])
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("line %d: invalid key format", lineNo)
		}

		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("line %d: duplicate key %s", lineNo, key)
		}

		values[key] = unquoteEnvValue(strings.TrimSpace(line[idx+1:]))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	return values, nil
}

func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
