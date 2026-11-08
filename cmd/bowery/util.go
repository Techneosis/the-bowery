package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// jsonOnce is a --json command's promise of exactly one line on stdout:
// once on, the first emitJSON prints it, and a die before then prints a
// "refused" line of its own.
var jsonOnce struct {
	on, done bool
	name     string
}

func emitJSON(fields []kv) {
	if jsonOnce.done {
		return
	}
	jsonOnce.done = true
	fmt.Println(jsonLine(fields))
}

func die(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if jsonOnce.on && !jsonOnce.done {
		emitJSON([]kv{{"name", jsonOnce.name}, {"outcome", "refused"}, {"why", msg}})
	}
	fmt.Fprintf(os.Stderr, "bowery: %s\n", msg)
	os.Exit(1)
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bowery: "+format+"\n", args...)
}

func say(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		die("cannot find the home directory: %v", err)
	}
	return h
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return home() + p[1:]
	}
	return p
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return a
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

type kv struct{ k, v string }

// jsonLine is a flat JSON object in the given key order, spaced as Python's
// json.dumps spaces it (the shape orchestrators already parse).
func jsonLine(pairs []kv) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = strconv.Quote(p.k) + ": " + jsonString(p.v)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// maxTimeoutMS is 24 hours: past that, a deadline in a time.Duration is no
// longer far off but wraps into the past.
const maxTimeoutMS = 24 * 60 * 60 * 1000

// timeoutArg checks a --timeout value: milliseconds, a whole number above 0.
// It returns the number as herdr is given it, never the raw word.
func timeoutArg(v string) string {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > maxTimeoutMS {
		die("--timeout takes milliseconds, a whole number from 1 to %d (24 hours) (got '%s')", maxTimeoutMS, v)
	}
	return strconv.Itoa(n)
}
