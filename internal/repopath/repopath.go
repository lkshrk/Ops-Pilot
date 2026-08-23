// Package repopath answers whether a path is a plain path inside a repository.
package repopath

import "strings"

const MaxLength = 4096

type Fault int

const (
	OK Fault = iota
	Empty
	TooLong
	NotRelative
	NotPlain
	escapes
)

func Plain(path string) bool { return Check(path) == OK }

func Check(path string) Fault {
	switch {
	case path == "":
		return Empty
	case len(path) > MaxLength:
		return TooLong
	case strings.HasPrefix(path, "/"), strings.HasSuffix(path, "/"):
		return NotRelative
	case strings.Contains(path, "//"), strings.ContainsAny(path, "\x00\n\r"):
		return NotPlain
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return escapes
		}
	}
	return OK
}
