package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	testCases := []struct {
		input    string
		expected slog.Level
	}{
		{"DEBUG", slog.LevelDebug},
		{"debug", slog.LevelDebug},
		{"Debug", slog.LevelDebug},
		{"INFO", slog.LevelInfo},
		{"info", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"warn", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"error", slog.LevelError},
		{"FATAL", LevelFatal},
		{"fatal", LevelFatal},
		{"INVALID", slog.LevelInfo}, // default fallback
		{"", slog.LevelInfo},        // default fallback
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := ParseLevel(tc.input)
			if result != tc.expected {
				t.Errorf("parseLevel(%q) = %v, want %v", tc.input, result, tc.expected)
			}
		})
	}
}

func TestInit_JSONFormat(t *testing.T) {
	output := captureStdout(t, func() {
		Init(LogConfig{Level: "INFO", Format: "json"})
		slog.Info("test message")
	})

	if !strings.Contains(output, `"level":"info"`) {
		t.Error("Expected JSON formatted log output")
	}
	if !strings.Contains(output, `"message":"test message"`) {
		t.Error("Expected message in JSON output")
	}
}

// TestInit_JSONFieldSemantics pins the field names log collectors are already
// configured for: "time", "level" (lower case), "message", and errors rendered
// as strings under an "error" key.
func TestInit_JSONFieldSemantics(t *testing.T) {
	output := captureStdout(t, func() {
		Init(LogConfig{Level: "INFO", Format: "json"})
		slog.Error("boom", "error", errors.New("disk on fire"), "count", 3)
	})

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &record); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, output)
	}

	if record["level"] != "error" {
		t.Errorf(`level = %v, want "error"`, record["level"])
	}
	if record["message"] != "boom" {
		t.Errorf(`message = %v, want "boom"`, record["message"])
	}
	if record["error"] != "disk on fire" {
		t.Errorf(`error = %v, want "disk on fire"`, record["error"])
	}
	if record["count"] != float64(3) {
		t.Errorf("count = %v, want 3", record["count"])
	}
	if _, ok := record["time"]; !ok {
		t.Error("expected a time field")
	}
	if _, ok := record["msg"]; ok {
		t.Error(`slog's "msg" key leaked; it must be renamed to "message"`)
	}
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what was
// written. Init binds the handler to os.Stdout, so the swap must happen first.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = original

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestInit_ConsoleFormat(t *testing.T) {
	output := captureStdout(t, func() {
		Init(LogConfig{Level: "DEBUG", Format: "console", Color: false})
		slog.Info("console test message")
	})

	if !strings.Contains(output, "console test message") {
		t.Error("Expected console formatted log output")
	}
	// Should contain a timestamp.
	if !strings.Contains(output, ":") {
		t.Error("Expected timestamp in console output")
	}
}

// TestConsole_QuotesControlCharacters: a value carrying a newline (a decoded
// %0A in a request path, say) renders quoted on one line, so it cannot forge a
// second log record.
func TestConsole_QuotesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	slog.New(newConsoleHandler(&buf, false)).Info("request",
		"path", "/simple/x\nINF\x1bforged", "tab", "a\tb", "eq", "a=b", "plain", "ok")

	out := buf.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("want one line, got %d: %q", n, out)
	}
	for _, want := range []string{`path="/simple/x\nINF\x1bforged"`, `tab="a\tb"`, `eq="a=b"`, ` plain=ok`} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in %q", want, out)
		}
	}
}

// TestConsole_QuotesNonPrintable: any non-printable rune (a terminal escape, a
// NUL, a non-breaking space) renders quoted, so a value cannot drive the
// terminal it is printed on.
func TestConsole_QuotesNonPrintable(t *testing.T) {
	for _, v := range []string{"\x1b[2J", "\x00", " "} {
		var buf bytes.Buffer
		slog.New(newConsoleHandler(&buf, false)).Info("m", "v", v)
		if want := " v=" + strconv.Quote(v); !strings.Contains(buf.String(), want) {
			t.Errorf("want %s in %q", want, buf.String())
		}
	}
}

// TestConsole_GroupDoesNotPrefixEarlierAttrs: attrs added before WithGroup keep
// their own (empty) prefix; only later attrs are grouped.
func TestConsole_GroupDoesNotPrefixEarlierAttrs(t *testing.T) {
	var buf bytes.Buffer
	slog.New(newConsoleHandler(&buf, false)).With("a", 1).WithGroup("g").With("c", 3).Info("m", "b", 2)
	out := buf.String()
	for _, want := range []string{" a=1", " g.c=3", " g.b=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in %q", want, out)
		}
	}
	if strings.Contains(out, "g.a=1") {
		t.Errorf("a=1 was added before the group but got prefixed: %q", out)
	}
}

func TestInit_LevelFiltering(t *testing.T) {
	output := captureStdout(t, func() {
		Init(LogConfig{Level: "WARN", Format: "console", Color: false})
		slog.Debug("debug message")
		slog.Info("info message")
		slog.Warn("warn message")
		slog.Error("error message")
	})

	if strings.Contains(output, "debug message") {
		t.Error("Debug message should be filtered out at WARN level")
	}
	if strings.Contains(output, "info message") {
		t.Error("Info message should be filtered out at WARN level")
	}
	if !strings.Contains(output, "warn message") {
		t.Error("Warn message should be included at WARN level")
	}
	if !strings.Contains(output, "error message") {
		t.Error("Error message should be included at WARN level")
	}
}

// TestInit_WarnsOnUnknownValue: a mistyped level or format still starts the
// proxy on the default, but says so, the way config announces any other
// malformed setting — or an operator who wrote "verbose" never learns why debug
// lines are missing.
func TestInit_WarnsOnUnknownValue(t *testing.T) {
	output := captureStdout(t, func() {
		Init(LogConfig{Level: "verbose", Format: "xml"})
	})
	for _, want := range []string{"variable=GROXPI_LOGGING_LEVEL value=verbose", "variable=GROXPI_LOG_FORMAT value=xml"} {
		if !strings.Contains(output, want) {
			t.Errorf("expected a warning containing %q, got %q", want, output)
		}
	}

	quiet := captureStdout(t, func() {
		Init(LogConfig{Level: "warning", Format: "JSON"})
		Init(LogConfig{Level: "", Format: ""})
	})
	if quiet != "" {
		t.Errorf("known or empty values must not warn, got %q", quiet)
	}
}

func TestInit_DefaultFormat(t *testing.T) {
	// An empty format falls back to console.
	Init(LogConfig{Level: "INFO", Format: "", Color: false})

	ctx := context.Background()
	if !slog.Default().Enabled(ctx, slog.LevelInfo) {
		t.Error("Logger should be enabled at INFO")
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("Logger should not be enabled at DEBUG when configured for INFO")
	}
}

// TestInit_ColorConfiguration pins the colour setting, which is a spec
// requirement: with Color set the console line carries the ANSI level tag,
// without it the same line is plain.
func TestInit_ColorConfiguration(t *testing.T) {
	coloured := captureStdout(t, func() {
		Init(LogConfig{Level: "INFO", Format: "console", Color: true})
		slog.Info("hello")
	})
	if !strings.Contains(coloured, "\x1b[32mINF\x1b[0m") {
		t.Errorf("expected a coloured level tag, got %q", coloured)
	}

	plain := captureStdout(t, func() {
		Init(LogConfig{Level: "INFO", Format: "console", Color: false})
		slog.Info("hello")
	})
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("expected no escape sequences with Color disabled, got %q", plain)
	}
	if !strings.Contains(plain, "INF hello") {
		t.Errorf("expected a plain level tag, got %q", plain)
	}
}
