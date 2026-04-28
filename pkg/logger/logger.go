// Package logger provides structured, leveled logging with hierarchical prefixes.
//
// Log Format: [LEVEL] [SERVICE.MODULE.OPERATION] message key=value ...
//
// Levels: DEBUG < INFO < WARN < ERROR < FATAL
//
// Usage:
//
//	log := logger.New("AUTH", "OTP")
//	log.Info("SEND", "OTP sent successfully", "phone", "+91xxxxx", "otp_id", "abc123")
//	log.Error("VERIFY", "OTP verification failed", "phone", "+91xxxxx", "err", err)
//	log.WithFields("request_id", reqID).Info("SEND", "processing request")
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Level represents the severity of a log message
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

var levelNames = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
	LevelFatal: "FATAL",
}

var levelColors = map[Level]string{
	LevelDebug: "\033[36m",  // Cyan
	LevelInfo:  "\033[32m",  // Green
	LevelWarn:  "\033[33m",  // Yellow
	LevelError: "\033[31m",  // Red
	LevelFatal: "\033[35m",  // Magenta
}

const colorReset = "\033[0m"

// Global configuration
var (
	globalLevel   Level = LevelDebug
	globalOutput  io.Writer = os.Stdout
	globalColor   bool = true
	globalJSON    bool = false
	mu            sync.RWMutex
)

// SetLevel sets the global minimum log level
func SetLevel(level Level) {
	mu.Lock()
	defer mu.Unlock()
	globalLevel = level
}

// SetLevelFromString sets log level from string ("debug", "info", "warn", "error", "fatal")
func SetLevelFromString(s string) {
	switch strings.ToLower(s) {
	case "debug":
		SetLevel(LevelDebug)
	case "info":
		SetLevel(LevelInfo)
	case "warn", "warning":
		SetLevel(LevelWarn)
	case "error":
		SetLevel(LevelError)
	case "fatal":
		SetLevel(LevelFatal)
	default:
		SetLevel(LevelInfo)
	}
}

// SetOutput sets the global log output writer
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	globalOutput = w
}

// SetColorEnabled enables/disables color output
func SetColorEnabled(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	globalColor = enabled
}

// SetJSONMode enables/disables JSON output format
func SetJSONMode(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	globalJSON = enabled
}

// Logger is a structured logger with hierarchical prefix context
type Logger struct {
	module    string // e.g., "AUTH"
	subModule string // e.g., "OTP"
	fields    []interface{} // persistent key-value pairs
}

// New creates a new Logger with module and sub-module prefix
// Example: logger.New("AUTH", "OTP") → [AUTH.OTP]
func New(module, subModule string) *Logger {
	return &Logger{
		module:    strings.ToUpper(module),
		subModule: strings.ToUpper(subModule),
	}
}

// WithFields returns a new Logger with additional persistent key-value fields
func (l *Logger) WithFields(kvs ...interface{}) *Logger {
	newFields := make([]interface{}, len(l.fields)+len(kvs))
	copy(newFields, l.fields)
	copy(newFields[len(l.fields):], kvs)
	return &Logger{
		module:    l.module,
		subModule: l.subModule,
		fields:    newFields,
	}
}

// Debug logs at DEBUG level — detailed diagnostic information
func (l *Logger) Debug(operation, msg string, kvs ...interface{}) {
	l.log(LevelDebug, operation, msg, kvs...)
}

// Info logs at INFO level — normal operational messages
func (l *Logger) Info(operation, msg string, kvs ...interface{}) {
	l.log(LevelInfo, operation, msg, kvs...)
}

// Warn logs at WARN level — potential issues or unexpected behavior
func (l *Logger) Warn(operation, msg string, kvs ...interface{}) {
	l.log(LevelWarn, operation, msg, kvs...)
}

// Error logs at ERROR level — errors that need attention
func (l *Logger) Error(operation, msg string, kvs ...interface{}) {
	l.log(LevelError, operation, msg, kvs...)
}

// Fatal logs at FATAL level and exits the process
func (l *Logger) Fatal(operation, msg string, kvs ...interface{}) {
	l.log(LevelFatal, operation, msg, kvs...)
	os.Exit(1)
}

// ErrorWithCode logs an error with an error code reference
func (l *Logger) ErrorWithCode(operation string, code string, msg string, kvs ...interface{}) {
	allKVs := append([]interface{}{"error_code", code}, kvs...)
	l.log(LevelError, operation, msg, allKVs...)
}

// WarnWithCode logs a warning with an error code reference
func (l *Logger) WarnWithCode(operation string, code string, msg string, kvs ...interface{}) {
	allKVs := append([]interface{}{"error_code", code}, kvs...)
	l.log(LevelWarn, operation, msg, allKVs...)
}

func (l *Logger) log(level Level, operation, msg string, kvs ...interface{}) {
	mu.RLock()
	minLevel := globalLevel
	output := globalOutput
	useColor := globalColor
	useJSON := globalJSON
	mu.RUnlock()

	if level < minLevel {
		return
	}

	// Build prefix: [MODULE.SUBMODULE.OPERATION]
	prefix := l.module
	if l.subModule != "" {
		prefix += "." + l.subModule
	}
	if operation != "" {
		prefix += "." + strings.ToUpper(operation)
	}

	// Merge persistent fields with call-specific kvs
	allKVs := make([]interface{}, 0, len(l.fields)+len(kvs))
	allKVs = append(allKVs, l.fields...)
	allKVs = append(allKVs, kvs...)

	timestamp := time.Now().Format("2006-01-02T15:04:05.000Z07:00")

	if useJSON {
		l.logJSON(output, level, timestamp, prefix, msg, allKVs)
	} else {
		l.logText(output, level, timestamp, prefix, msg, allKVs, useColor)
	}
}

func (l *Logger) logText(w io.Writer, level Level, ts, prefix, msg string, kvs []interface{}, color bool) {
	levelStr := levelNames[level]

	var sb strings.Builder
	sb.WriteString(ts)
	sb.WriteString(" ")

	if color {
		sb.WriteString(levelColors[level])
		sb.WriteString(fmt.Sprintf("%-5s", levelStr))
		sb.WriteString(colorReset)
	} else {
		sb.WriteString(fmt.Sprintf("%-5s", levelStr))
	}

	sb.WriteString(" [")
	sb.WriteString(prefix)
	sb.WriteString("] ")
	sb.WriteString(msg)

	// Append key=value pairs
	for i := 0; i+1 < len(kvs); i += 2 {
		key := fmt.Sprintf("%v", kvs[i])
		val := kvs[i+1]
		sb.WriteString(" ")
		sb.WriteString(key)
		sb.WriteString("=")

		// Handle error type specially
		if err, ok := val.(error); ok {
			sb.WriteString(fmt.Sprintf("%q", err.Error()))
		} else {
			sb.WriteString(fmt.Sprintf("%v", val))
		}
	}

	sb.WriteString("\n")
	fmt.Fprint(w, sb.String())
}

func (l *Logger) logJSON(w io.Writer, level Level, ts, prefix, msg string, kvs []interface{}) {
	var sb strings.Builder
	sb.WriteString(`{"ts":"`)
	sb.WriteString(ts)
	sb.WriteString(`","level":"`)
	sb.WriteString(levelNames[level])
	sb.WriteString(`","prefix":"`)
	sb.WriteString(prefix)
	sb.WriteString(`","msg":"`)
	sb.WriteString(escapeJSON(msg))
	sb.WriteString(`"`)

	for i := 0; i+1 < len(kvs); i += 2 {
		key := fmt.Sprintf("%v", kvs[i])
		val := kvs[i+1]
		sb.WriteString(`,"`)
		sb.WriteString(escapeJSON(key))
		sb.WriteString(`":"`)
		if err, ok := val.(error); ok {
			sb.WriteString(escapeJSON(err.Error()))
		} else {
			sb.WriteString(escapeJSON(fmt.Sprintf("%v", val)))
		}
		sb.WriteString(`"`)
	}

	sb.WriteString("}\n")
	fmt.Fprint(w, sb.String())
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// Init initializes the global logger settings from environment/config
// Call this once in main() before any logging.
func Init(level string, jsonMode bool, colorEnabled bool) {
	SetLevelFromString(level)
	SetJSONMode(jsonMode)
	SetColorEnabled(colorEnabled)

	// Redirect stdlib log to our output
	log.SetOutput(globalOutput)
	log.SetFlags(0) // We handle timestamps ourselves
}
