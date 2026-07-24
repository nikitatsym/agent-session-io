package sessionio

import (
	"fmt"
	"strconv"
	"strings"
)

// ReaderError adds reader context while preserving the underlying error.
type ReaderError struct {
	Operation      string
	Harness        Harness
	AdapterVersion string
	SessionID      SessionID
	Locator        *SourceLocator
	Err            error
}

func (readerError *ReaderError) Error() string {
	if readerError == nil {
		return "sessionio: reader error"
	}

	parts := []string{"sessionio: reader error"}
	if readerError.Operation != "" {
		parts = append(parts, "operation="+readerError.Operation)
	}
	if readerError.Harness != "" {
		parts = append(parts, "harness="+string(readerError.Harness))
	}
	if readerError.AdapterVersion != "" {
		parts = append(parts, "adapter_version="+readerError.AdapterVersion)
	}
	if readerError.SessionID != "" {
		parts = append(parts, "session_id="+string(readerError.SessionID))
	}
	if readerError.Locator != nil {
		parts = append(parts, "locator="+formatLocator(*readerError.Locator))
	}
	if readerError.Err != nil {
		parts = append(parts, "cause="+readerError.Err.Error())
	}
	return strings.Join(parts, " ")
}

// Unwrap returns the underlying reader failure.
func (readerError *ReaderError) Unwrap() error {
	if readerError == nil {
		return nil
	}
	return readerError.Err
}

func formatLocator(locator SourceLocator) string {
	switch locator.Kind {
	case LocatorKindFile:
		if locator.File == nil {
			return string(locator.Kind)
		}
		parts := []string{fmt.Sprintf("%s:%s/%s", locator.Kind, locator.File.Root, locator.File.Path)}
		if locator.File.Record != nil {
			parts = append(parts, "record="+strconv.FormatUint(*locator.File.Record, 10))
		}
		if locator.File.Line != nil {
			parts = append(parts, "line="+strconv.FormatUint(*locator.File.Line, 10))
		}
		if locator.File.ByteRange != nil {
			parts = append(
				parts,
				fmt.Sprintf(
					"bytes=%d-%d",
					locator.File.ByteRange.Start,
					locator.File.ByteRange.End,
				),
			)
		}
		return strings.Join(parts, ",")
	case LocatorKindDatabase:
		if locator.Database == nil {
			return string(locator.Kind)
		}
		parts := []string{
			fmt.Sprintf("%s:%s:%s", locator.Kind, locator.Database.Path, locator.Database.Table),
		}
		for _, key := range locator.Database.Keys {
			parts = append(parts, key.Name+"="+key.Value)
		}
		return strings.Join(parts, ",")
	case LocatorKindOpaque:
		if locator.Opaque == nil {
			return string(locator.Kind)
		}
		return fmt.Sprintf("%s:%s:%s", locator.Kind, locator.Opaque.Scheme, locator.Opaque.Value)
	default:
		return string(locator.Kind)
	}
}
