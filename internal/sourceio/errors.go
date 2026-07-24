package sourceio

import (
	"crypto/sha256"
	"fmt"

	sessionio "github.com/nikitatsym/agent-session-io"
)

// RecordTooLargeError reports an explicit record-size policy violation.
type RecordTooLargeError struct {
	Locator         sessionio.SourceLocator
	Limit           int64
	ObservedAtLeast int64
}

func (recordError *RecordTooLargeError) Error() string {
	if recordError == nil {
		return "sourceio: record exceeds size limit"
	}
	return fmt.Sprintf(
		"sourceio: record %s exceeds size limit: observed_at_least=%d limit=%d",
		formatSourceLocator(recordError.Locator),
		recordError.ObservedAtLeast,
		recordError.Limit,
	)
}

// MalformedJSONLError reports a complete record that is not one JSON value.
type MalformedJSONLError struct {
	Locator sessionio.SourceLocator
	Err     error
}

func (jsonError *MalformedJSONLError) Error() string {
	if jsonError == nil {
		return "sourceio: malformed JSONL record"
	}
	if jsonError.Err == nil {
		return "sourceio: malformed JSONL record " + formatSourceLocator(jsonError.Locator)
	}
	return fmt.Sprintf(
		"sourceio: malformed JSONL record %s: %v",
		formatSourceLocator(jsonError.Locator),
		jsonError.Err,
	)
}

// Unwrap returns the JSON parser failure.
func (jsonError *MalformedJSONLError) Unwrap() error {
	if jsonError == nil {
		return nil
	}
	return jsonError.Err
}

// ChangedSourceError reports bytes that changed between acquisition passes.
type ChangedSourceError struct {
	Locator        sessionio.SourceLocator
	ExpectedSHA256 [sha256.Size]byte
	ActualSHA256   [sha256.Size]byte
	Resume         ResumeToken
}

func (sourceError *ChangedSourceError) Error() string {
	if sourceError == nil {
		return "sourceio: source changed during verified read"
	}
	return fmt.Sprintf(
		"sourceio: source changed during verified read %s expected_sha256=%x actual_sha256=%x",
		formatSourceLocator(sourceError.Locator),
		sourceError.ExpectedSHA256,
		sourceError.ActualSHA256,
	)
}

func formatSourceLocator(locator sessionio.SourceLocator) string {
	if locator.Kind != sessionio.LocatorKindFile || locator.File == nil {
		return string(locator.Kind)
	}
	text := fmt.Sprintf("%s/%s", locator.File.Root, locator.File.Path)
	if locator.File.Record != nil {
		text += fmt.Sprintf(":record=%d", *locator.File.Record)
	}
	if locator.File.Line != nil {
		text += fmt.Sprintf(":line=%d", *locator.File.Line)
	}
	if locator.File.ByteRange != nil {
		text += fmt.Sprintf(
			":bytes=%d-%d",
			locator.File.ByteRange.Start,
			locator.File.ByteRange.End,
		)
	}
	return text
}
