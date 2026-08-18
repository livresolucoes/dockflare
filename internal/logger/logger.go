package logger

import (
	"fmt"
	"io"
	"os"
)

type Logger struct {
	out io.Writer
}

func New(out io.Writer) *Logger {
	return &Logger{out: out}
}

func (l *Logger) Info(format string, args ...any) {
	l.log("INFO", format, args...)
}

func (l *Logger) Warn(format string, args ...any) {
	l.log("WARN", format, args...)
}

func (l *Logger) Error(format string, args ...any) {
	l.log("ERROR", format, args...)
}

func (l *Logger) Fatal(format string, args ...any) {
	l.log("ERROR", format, args...)
	os.Exit(1)
}

func (l *Logger) log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "[%s] %s\n", level, msg)
}
