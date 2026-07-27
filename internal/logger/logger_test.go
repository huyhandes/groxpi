package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
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

func TestIsTerminal(t *testing.T) {
	// This test is environment dependent, so we just ensure the function doesn't panic
	// and returns a boolean value
	result := IsTerminal()
	if result != true && result != false {
		t.Error("isTerminal() should return a boolean value")
	}
}

func TestInit_JSONFormat(t *testing.T) {
	// Capture original stdout
	originalStdout := os.Stdout
	defer func() { os.Stdout = originalStdout }()

	// Create a pipe to capture output
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Initialize logger with JSON format
	cfg := LogConfig{
		Level:  "INFO",
		Format: "json",
	}
	Init(cfg)

	// Test logging
	Logger.Info("test message")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// Restore stdout
	os.Stdout = originalStdout

	// Verify JSON format (should contain structured JSON)
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
		Logger.Error("boom", "error", errors.New("disk on fire"), "count", 3)
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
	// Capture original stdout
	originalStdout := os.Stdout
	defer func() { os.Stdout = originalStdout }()

	// Create a pipe to capture output
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Initialize logger with console format
	cfg := LogConfig{
		Level:  "DEBUG",
		Format: "console",
		Color:  false, // Disable color for easier testing
	}
	Init(cfg)

	// Test logging
	Logger.Info("console test message")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// Restore stdout
	os.Stdout = originalStdout

	// Verify console format (should be human readable)
	if !strings.Contains(output, "console test message") {
		t.Error("Expected console formatted log output")
	}
	// Should contain timestamp
	if !strings.Contains(output, ":") {
		t.Error("Expected timestamp in console output")
	}
}

func TestInit_LevelFiltering(t *testing.T) {
	// Capture original stdout
	originalStdout := os.Stdout
	defer func() { os.Stdout = originalStdout }()

	// Create a pipe to capture output
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Initialize logger with WARN level
	cfg := LogConfig{
		Level:  "WARN",
		Format: "console",
		Color:  false,
	}
	Init(cfg)

	// Log at different levels
	Logger.Debug("debug message")
	Logger.Info("info message")
	Logger.Warn("warn message")
	Logger.Error("error message")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// Restore stdout
	os.Stdout = originalStdout

	// Should not contain debug and info messages
	if strings.Contains(output, "debug message") {
		t.Error("Debug message should be filtered out at WARN level")
	}
	if strings.Contains(output, "info message") {
		t.Error("Info message should be filtered out at WARN level")
	}

	// Should contain warn and error messages
	if !strings.Contains(output, "warn message") {
		t.Error("Warn message should be included at WARN level")
	}
	if !strings.Contains(output, "error message") {
		t.Error("Error message should be included at WARN level")
	}
}

func TestConvenienceFunctions(t *testing.T) {
	// Capture original stdout
	originalStdout := os.Stdout
	defer func() { os.Stdout = originalStdout }()

	// Create a pipe to capture output
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Initialize logger
	cfg := LogConfig{
		Level:  "DEBUG",
		Format: "console",
		Color:  false,
	}
	Init(cfg)

	// Test convenience functions
	Debug("debug test")
	Info("info test")
	Warn("warn test")
	Error("error test")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// Restore stdout
	os.Stdout = originalStdout

	// Verify all messages are present
	expectedMessages := []string{"debug test", "info test", "warn test", "error test"}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("Expected message %q not found in output", msg)
		}
	}
}

func TestGetLogger(t *testing.T) {
	// Initialize logger
	cfg := LogConfig{
		Level:  "INFO",
		Format: "json",
	}
	Init(cfg)

	// Get logger instance
	logger := GetLogger()
	if logger == nil {
		t.Error("GetLogger() returned nil")
	}

	// Verify it's the same instance
	if logger != Logger {
		t.Error("GetLogger() did not return the global Logger instance")
	}
}

func TestInit_DefaultFormat(t *testing.T) {
	// Test with empty format (should default to console)
	cfg := LogConfig{
		Level:  "INFO",
		Format: "",
		Color:  false,
	}

	// Should not panic
	Init(cfg)

	// Verify logger is initialized at the configured level
	ctx := context.Background()
	if !Logger.Enabled(ctx, slog.LevelInfo) {
		t.Error("Logger should be enabled at INFO")
	}
	if Logger.Enabled(ctx, slog.LevelDebug) {
		t.Error("Logger should not be enabled at DEBUG when configured for INFO")
	}
}

func TestInit_ColorConfiguration(t *testing.T) {
	// Test color enabled
	cfg := LogConfig{
		Level:  "INFO",
		Format: "console",
		Color:  true,
	}

	// Should not panic
	Init(cfg)

	// Test color disabled
	cfg.Color = false
	Init(cfg)

	// Both configurations should work without errors
}
