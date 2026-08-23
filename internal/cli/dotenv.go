package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var dotenvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func loadDotenv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open dotenv: %w", err)
	}
	defer file.Close()
	return parseDotenv(file)
}

func parseDotenv(r io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "export ") || strings.HasPrefix(text, "export\t") {
			text = strings.TrimSpace(text[len("export"):])
		}
		name, raw, found := strings.Cut(text, "=")
		if !found {
			return nil, dotenvSyntaxError(line, "missing =")
		}
		name = strings.TrimSpace(name)
		if !dotenvName.MatchString(name) {
			return nil, dotenvSyntaxError(line, "invalid name")
		}
		value, err := dotenvValue(strings.TrimSpace(raw), line)
		if err != nil {
			return nil, err
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read dotenv: %w", err)
	}
	return values, nil
}

func dotenvValue(raw string, line int) (string, error) {
	if raw == "" || (raw[0] != '\'' && raw[0] != '"') {
		return raw, nil
	}
	quote := raw[0]
	end := strings.IndexByte(raw[1:], quote)
	if end < 0 || strings.TrimSpace(raw[end+2:]) != "" {
		return "", dotenvSyntaxError(line, "invalid quote")
	}
	return raw[1 : end+1], nil
}

func dotenvSyntaxError(line int, category string) error {
	return fmt.Errorf("dotenv line %d: %s", line, category)
}

func environmentLookup(
	primary func(string) (string, bool),
	fallback map[string]string,
) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if value, present := primary(name); present {
			return value, true
		}
		value, present := fallback[name]
		return value, present
	}
}
