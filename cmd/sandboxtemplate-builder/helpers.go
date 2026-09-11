package main

import (
	"strings"

	"fast-sandbox/internal/artifacts"
)

// shellQuote single-quotes an argument for /bin/sh, escaping embedded
// single quotes.
func shellQuote(argument string) string {
	return "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
}

// sha256Of returns the hex digest of a byte slice.
func sha256Of(payload []byte) string {
	return artifacts.SHA256Of(payload)
}
