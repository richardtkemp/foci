package timeutil

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultUsesLocalTime(t *testing.T) {
	SetLocation(nil)
	defer SetLocation(nil)

	now := Now()
	_, offset := now.Zone()
	_, localOffset := time.Now().Zone()
	if offset != localOffset {
		t.Errorf("Now() offset = %d, want local offset %d", offset, localOffset)
	}
}

func TestSetLocationUTC(t *testing.T) {
	SetLocation(time.UTC)
	defer SetLocation(nil)

	s := Format(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))
	if !strings.HasSuffix(s, "Z") && !strings.HasSuffix(s, "+00:00") {
		t.Errorf("Format with UTC location = %q, want Z or +00:00 suffix", s)
	}
}

func TestSetLocationFixedOffset(t *testing.T) {
	tz := time.FixedZone("Test+3", 3*3600)
	SetLocation(tz)
	defer SetLocation(nil)

	s := Format(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(s, "+03:00") {
		t.Errorf("Format with +3 location = %q, want +03:00", s)
	}
	if !strings.Contains(s, "15:00:00") {
		t.Errorf("Format with +3 location = %q, want 15:00:00 (12 UTC + 3)", s)
	}
}

func TestFormatNanoIncludesOffset(t *testing.T) {
	tz := time.FixedZone("Test+5", 5*3600)
	SetLocation(tz)
	defer SetLocation(nil)

	s := FormatNano(time.Date(2026, 1, 15, 10, 30, 0, 123456789, time.UTC))
	if !strings.Contains(s, "+05:00") {
		t.Errorf("FormatNano = %q, want +05:00 offset", s)
	}
}

func TestFormatFilenameNoColons(t *testing.T) {
	SetLocation(time.UTC)
	defer SetLocation(nil)

	s := FormatFilename(time.Date(2026, 4, 1, 14, 30, 45, 0, time.UTC))
	if strings.Contains(s, ":") {
		t.Errorf("FormatFilename = %q, should not contain colons", s)
	}
	if s != "2026-04-01T14-30-45+0000" {
		t.Errorf("FormatFilename = %q, want 2026-04-01T14-30-45+0000", s)
	}
}

func TestNowUsesConfiguredLocation(t *testing.T) {
	tz := time.FixedZone("Test-8", -8*3600)
	SetLocation(tz)
	defer SetLocation(nil)

	now := Now()
	name, _ := now.Zone()
	if name != "Test-8" {
		t.Errorf("Now().Zone() = %q, want Test-8", name)
	}
}
