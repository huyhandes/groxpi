package logger

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/phuslu/log"
)

func TestParseLevel(t *testing.T) {
	testCases := []struct {
		input    string
		expected log.Level
	}{
		{"DEBUG", log.DebugLevel},
		{"debug", log.DebugLevel},
		{"Debug", log.DebugLevel},
		{"INFO", log.InfoLevel},
		{"info", log.InfoLevel},
		{"WARN", log.WarnLevel},
		{"WARNING", log.WarnLevel},
		{"warn", log.WarnLevel},
		{"ERROR", log.ErrorLevel},
		{"error", log.ErrorLevel},
		{"FATAL", log.FatalLevel},
		{"fatal", log.FatalLevel},
		{"INVALID", log.InfoLevel}, // default fallback
		{"", log.InfoLevel},        // default fallback
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := parseLevel(tc.input)
			if result != tc.expected {
				t.Errorf("parseLevel(%q) = %v, want %v", tc.input, result, tc.expected)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	// This test is environment dependent, so we just ensure the function doesn't panic
	// and returns a boolean value
	result := isTerminal()
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
	Logger.Info().Msg("test message")

	// Close writer and read output
	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
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
	Logger.Info().Msg("console test message")

	// Close writer and read output
	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
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
	Logger.Debug().Msg("debug message")
	Logger.Info().Msg("info message")
	Logger.Warn().Msg("warn message")
	Logger.Error().Msg("error message")

	// Close writer and read output
	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
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
	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
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
	if logger != &Logger {
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

	// Verify logger is initialized
	if Logger.Level != log.InfoLevel {
		t.Error("Logger level not set correctly")
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
