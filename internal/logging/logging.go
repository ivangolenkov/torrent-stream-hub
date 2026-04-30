package logging

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelOff
)

var (
	defaultLogger = &Logger{
		level:  LevelDebug,
		logger: log.New(os.Stdout, "", log.LstdFlags),
	}
	sensitiveQueryRe = regexp.MustCompile(`(?i)(^|[?&\s])(passkey|token|apikey|api_key|key|auth|secret|sid|session|signature)=([^&\s]+)`)
)

type Logger struct {
	mu     sync.RWMutex
	level  Level
	logger *log.Logger
}

func Configure(level string) {
	defaultLogger.SetLevel(ParseLevel(level))
}

func CurrentLevel() string {
	return defaultLogger.LevelName()
}

func ParseLevel(raw string) Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "debug", "all", "trace":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	case "off", "none", "disabled":
		return LevelOff
	default:
		return LevelDebug
	}
}

func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

func (l *Logger) LevelName() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return levelName(l.level)
}

func Debugf(format string, args ...any) {
	defaultLogger.logf(LevelDebug, "DEBUG", format, args...)
}

func Infof(format string, args ...any) {
	defaultLogger.logf(LevelInfo, "INFO", format, args...)
}

func Warnf(format string, args ...any) {
	defaultLogger.logf(LevelWarn, "WARN", format, args...)
}

func Errorf(format string, args ...any) {
	defaultLogger.logf(LevelError, "ERROR", format, args...)
}

func IsDebugEnabled() bool {
	return defaultLogger.enabled(LevelDebug)
}

func (l *Logger) logf(level Level, label, format string, args ...any) {
	if !l.enabled(level) {
		return
	}

	message := SanitizeText(fmt.Sprintf(format, args...))
	l.mu.RLock()
	defer l.mu.RUnlock()
	l.logger.Printf("[%s] %s", label, message)
}

func (l *Logger) enabled(level Level) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level <= level && l.level != LevelOff
}

func levelName(level Level) string {
	switch level {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	case LevelOff:
		return "off"
	default:
		return "debug"
	}
}

func SanitizeText(text string) string {
	return sensitiveQueryRe.ReplaceAllString(text, "$1$2=<redacted>")
}

func SafeURLSummary(raw string) string {
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" {
		if parsed.Host != "" {
			return SanitizeText(parsed.Scheme + "://" + parsed.Host)
		}
		if parsed.Scheme == "magnet" {
			return SafeMagnetSummary(raw)
		}
	}

	safe := SanitizeText(raw)
	if len(safe) > 80 {
		return safe[:80] + "..."
	}
	return safe
}

func SafeMagnetSummary(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "magnet" {
		return SafeURLSummary(raw)
	}

	query := parsed.Query()
	infoHash := "unknown"
	if xt := query.Get("xt"); strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
		infoHash = xt[len("urn:btih:"):]
	}

	return fmt.Sprintf("magnet hash=%s trackers=%d", infoHash, len(query["tr"]))
}
