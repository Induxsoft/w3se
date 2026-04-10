package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelError = "error"
)

type Logger struct {
	level  int
	format string
	writer io.Writer
	mu     sync.Mutex
}

func NewLogger(cfg LoggingConfig) (*Logger, error) {
	var w io.Writer
	switch cfg.Output {
	case "stdout", "":
		w = os.Stdout
	default:
		f, err := os.OpenFile(cfg.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("cannot open log file %s: %w", cfg.Output, err)
		}
		w = f
	}

	level := 1
	switch cfg.Level {
	case LevelDebug:
		level = 0
	case LevelInfo, "":
		level = 1
	case LevelError:
		level = 2
	}

	format := cfg.Format
	if format == "" {
		format = "json"
	}

	return &Logger{level: level, format: format, writer: w}, nil
}

func (l *Logger) Debug(msg string, fields ...any) {
	if l.level <= 0 {
		l.log(LevelDebug, msg, fields...)
	}
}

func (l *Logger) Info(msg string, fields ...any) {
	if l.level <= 1 {
		l.log(LevelInfo, msg, fields...)
	}
}

func (l *Logger) Error(msg string, fields ...any) {
	if l.level <= 2 {
		l.log(LevelError, msg, fields...)
	}
}

func (l *Logger) log(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.format == "json" {
		entry := map[string]any{
			"time":  time.Now().UTC().Format(time.RFC3339Nano),
			"level": level,
			"msg":   msg,
		}
		for i := 0; i+1 < len(fields); i += 2 {
			key, ok := fields[i].(string)
			if ok {
				entry[key] = fields[i+1]
			}
		}
		data, _ := json.Marshal(entry)
		fmt.Fprintln(l.writer, string(data))
	} else {
		extra := ""
		for i := 0; i+1 < len(fields); i += 2 {
			extra += fmt.Sprintf(" %v=%v", fields[i], fields[i+1])
		}
		fmt.Fprintf(l.writer, "%s [%s] %s%s\n",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			level, msg, extra)
	}
}
