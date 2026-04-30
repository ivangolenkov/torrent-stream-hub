package logging

import (
	"strings"
	"testing"
)

func TestSanitizeTextRedactsSensitiveQueryValues(t *testing.T) {
	input := "tracker error url=http://tracker.example/announce?passkey=secret123&token=abc&info_hash=ok"
	got := SanitizeText(input)

	if strings.Contains(got, "secret123") || strings.Contains(got, "token=abc") {
		t.Fatalf("expected sensitive values to be redacted, got %q", got)
	}
	if !strings.Contains(got, "passkey=<redacted>") || !strings.Contains(got, "token=<redacted>") {
		t.Fatalf("expected redacted placeholders, got %q", got)
	}
	if !strings.Contains(got, "info_hash=ok") {
		t.Fatalf("expected non-sensitive query values to remain, got %q", got)
	}
}

func TestSafeURLSummaryDoesNotExposeQuery(t *testing.T) {
	got := SafeURLSummary("http://tracker.example/announce?passkey=secret123")
	if got != "http://tracker.example" {
		t.Fatalf("expected tracker URL summary, got %q", got)
	}
}

func TestSafeMagnetSummaryDoesNotExposeTrackerQueries(t *testing.T) {
	got := SafeMagnetSummary("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&tr=http%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3Dsecret")
	if strings.Contains(got, "secret") || strings.Contains(got, "tracker.example") {
		t.Fatalf("expected magnet summary without tracker details, got %q", got)
	}
	if !strings.Contains(got, "0123456789abcdef0123456789abcdef01234567") || !strings.Contains(got, "trackers=1") {
		t.Fatalf("expected hash and tracker count, got %q", got)
	}
}

func TestParseLevelDefaultsToDebug(t *testing.T) {
	if got := ParseLevel("unexpected"); got != LevelDebug {
		t.Fatalf("expected invalid level to default to debug, got %v", got)
	}
}
