package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
	"sync"
)

// UseFiles installs the shared destination. Close it after all producers stop.
func UseFiles(files *Files) func() {
	oldSlog := slog.Default()
	oldOutput, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	defaultLogger.mu.Lock()
	oldDestination := defaultLogger.logger.Writer()
	defaultLogger.logger.SetOutput(files)
	defaultLogger.mu.Unlock()
	slog.SetDefault(SlogLogger())
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(standardWriter{})
	var once sync.Once
	return func() {
		once.Do(func() {
			slog.SetDefault(oldSlog)
			log.SetOutput(oldOutput)
			log.SetFlags(oldFlags)
			log.SetPrefix(oldPrefix)
			setDestination(oldDestination)
		})
	}
}

func setDestination(w io.Writer) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.logger.SetOutput(w)
}

type standardWriter struct{}

func (standardWriter) Write(p []byte) (int, error) {
	Infof("%s", strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// SlogLogger bridges library logs without enabling their verbose debug tracing.
func SlogLogger() *slog.Logger { return slog.New(slogBridge{}) }

type slogBridge struct {
	attrs []slog.Attr
	group string
}

func slogLevel(level slog.Level) Level {
	if level >= slog.LevelError {
		return LevelError
	}
	if level >= slog.LevelWarn {
		return LevelWarn
	}
	if level >= slog.LevelInfo {
		return LevelInfo
	}
	return LevelDebug
}

func (h slogBridge) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo && defaultLogger.enabled(slogLevel(level))
}

func (h slogBridge) Handle(_ context.Context, record slog.Record) error {
	var text strings.Builder
	text.WriteString(record.Message)
	var appendAttr func(slog.Attr)
	appendAttr = func(attr slog.Attr) {
		value := attr.Value.Resolve()
		if value.Kind() == slog.KindGroup {
			for _, child := range value.Group() {
				if attr.Key != "" {
					child.Key = attr.Key + "." + child.Key
				}
				appendAttr(child)
			}
			return
		}
		if attr.Key == "" && value.Equal(slog.Value{}) {
			return
		}
		// Quote string attributes so masking also covers values containing spaces.
		if value.Kind() == slog.KindString {
			fmt.Fprintf(&text, " %s=%q", attr.Key, value.String())
		} else {
			fmt.Fprintf(&text, " %s=%v", attr.Key, value)
		}
	}
	for _, attr := range h.attrs {
		appendAttr(attr)
	}
	record.Attrs(func(attr slog.Attr) bool { attr.Key = h.group + attr.Key; appendAttr(attr); return true })
	level := slogLevel(record.Level)
	defaultLogger.logf(level, strings.ToUpper(levelName(level)), "%s", text.String())
	return nil
}

func (h slogBridge) WithAttrs(attrs []slog.Attr) slog.Handler {
	copyAttrs := append([]slog.Attr(nil), h.attrs...)
	for _, attr := range attrs {
		attr.Key = h.group + attr.Key
		copyAttrs = append(copyAttrs, attr)
	}
	h.attrs = copyAttrs
	return h
}

func (h slogBridge) WithGroup(name string) slog.Handler {
	if name != "" {
		h.group += name + "."
	}
	return h
}
