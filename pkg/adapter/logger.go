package adapter

import (
	"log"
	"strings"
)

type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarning
	LevelError
)

type Logger struct {
	level LogLevel
}

func NewLogger(level string, verbose bool) *Logger {
	lv := parseLogLevel(level)
	if verbose {
		lv = LevelDebug
	}
	return &Logger{level: lv}
}

func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "warning":
		return LevelWarning
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func (l *Logger) Enabled(level LogLevel) bool {
	return level >= l.level
}

func (l *Logger) Debugf(format string, args ...interface{}) {
	if l.Enabled(LevelDebug) {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func (l *Logger) Infof(format string, args ...interface{}) {
	if l.Enabled(LevelInfo) {
		log.Printf("[INFO] "+format, args...)
	}
}

func (l *Logger) Warnf(format string, args ...interface{}) {
	if l.Enabled(LevelWarning) {
		log.Printf("[WARNING] "+format, args...)
	}
}

func (l *Logger) Errorf(format string, args ...interface{}) {
	if l.Enabled(LevelError) {
		log.Printf("[ERROR] "+format, args...)
	}
}
