package logger

import (
	"os"
	"strings"
	"time"

	"github.com/phuslu/log"
)

var (
	// Global logger instance
	Logger log.Logger
)

// LogConfig holds logging configuration
type LogConfig struct {
	Level      string // DEBUG, INFO, WARN, ERROR
	Format     string // console, json
	TimeFormat string // time format for console output
	Color      bool   // enable color output for console
}

// Init initializes the global logger
func Init(cfg LogConfig) {
	// Set log level
	level := ParseLevel(cfg.Level)

	// Configure based on format
	switch strings.ToLower(cfg.Format) {
	case "json":
		// JSON format for log collectors
		Logger = log.Logger{
			Level:      level,
			TimeFormat: time.RFC3339,
			Writer: &log.IOWriter{
				Writer: os.Stdout,
			},
		}
	default:
		// Console format with optional color
		if cfg.Color || (cfg.Color && IsTerminal()) {
			// Color console output
			Logger = log.Logger{
				Level:      level,
				TimeFormat: "15:04:05.000",
				Writer: &log.ConsoleWriter{
					ColorOutput:    true,
					QuoteString:    true,
					EndWithMessage: true,
					Writer:         os.Stdout,
				},
			}
		} else {
			// Plain console output
			Logger = log.Logger{
				Level:      level,
				TimeFormat: "15:04:05.000",
				Writer: &log.ConsoleWriter{
					ColorOutput:    false,
					QuoteString:    true,
					EndWithMessage: true,
					Writer:         os.Stdout,
				},
			}
		}
	}

	// Set as default logger - this is crucial for log.Debug() calls throughout the codebase
	log.DefaultLogger = Logger
	
	// Also ensure the default logger level is set correctly
	log.DefaultLogger.SetLevel(level)
}

// parseLevel converts string level to log.Level
func ParseLevel(level string) log.Level {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return log.DebugLevel
	case "INFO":
		return log.InfoLevel
	case "WARN", "WARNING":
		return log.WarnLevel
	case "ERROR":
		return log.ErrorLevel
	case "FATAL":
		return log.FatalLevel
	default:
		return log.InfoLevel
	}
}

// isTerminal checks if stdout is a terminal
func IsTerminal() bool {
	fileInfo, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fileInfo.Mode() & os.ModeCharDevice) != 0
}

// Convenience functions for direct access

func Debug(msg string) {
	Logger.Debug().Msg(msg)
}

func Info(msg string) {
	Logger.Info().Msg(msg)
}

func Warn(msg string) {
	Logger.Warn().Msg(msg)
}

func Error(msg string) {
	Logger.Error().Msg(msg)
}

func Fatal(msg string) {
	Logger.Fatal().Msg(msg)
}

// GetLogger returns the global logger instance
func GetLogger() *log.Logger {
	return &Logger
}
