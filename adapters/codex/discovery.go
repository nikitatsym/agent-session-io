package codex

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
)

type occurrence struct {
	relative string
	active   bool
}

type discoveryResult struct {
	occurrences []occurrence
	compressed  []string
	diagnostics []sessionio.Diagnostic
	activeRoot  bool
	archiveRoot bool
}

func (adapter *Adapter) discover() (discoveryResult, error) {
	var result discoveryResult
	for _, root := range []struct {
		relative string
		active   bool
	}{
		{relative: "sessions", active: true},
		{relative: "archived_sessions"},
	} {
		path := filepath.Join(adapter.home, root.relative)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return discoveryResult{}, fmt.Errorf("stat rollout root %q: %w", path, err)
		}
		if root.active {
			result.activeRoot = true
		} else {
			result.archiveRoot = true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			result.addSymlinkDiagnostic(adapter.home, root.relative)
			continue
		}
		if !info.IsDir() {
			return discoveryResult{}, fmt.Errorf("Codex rollout root %q is not a directory", path)
		}
		if root.active {
			if err := adapter.scanActiveRoot(&result, path, root.relative); err != nil {
				return discoveryResult{}, err
			}
		} else if err := adapter.scanRolloutDirectory(&result, path, root.relative, false); err != nil {
			return discoveryResult{}, err
		}
	}
	sort.Slice(result.occurrences, func(left, right int) bool {
		return result.occurrences[left].relative < result.occurrences[right].relative
	})
	sort.Strings(result.compressed)
	return result, nil
}

func (adapter *Adapter) scanActiveRoot(result *discoveryResult, rootPath, rootRelative string) error {
	years, err := os.ReadDir(rootPath)
	if err != nil {
		return fmt.Errorf("read active rollout root %q: %w", rootPath, err)
	}
	for _, year := range years {
		if !validDatePart(year.Name(), 4) {
			continue
		}
		yearRelative := filepath.Join(rootRelative, year.Name())
		if skipSymlinkDirectory(result, adapter.home, year, yearRelative) || !year.IsDir() {
			continue
		}
		yearPath := filepath.Join(rootPath, year.Name())
		months, err := os.ReadDir(yearPath)
		if err != nil {
			return fmt.Errorf("read rollout year %q: %w", yearPath, err)
		}
		for _, month := range months {
			if !validDatePart(month.Name(), 2) {
				continue
			}
			monthRelative := filepath.Join(yearRelative, month.Name())
			if skipSymlinkDirectory(result, adapter.home, month, monthRelative) || !month.IsDir() {
				continue
			}
			monthPath := filepath.Join(yearPath, month.Name())
			days, err := os.ReadDir(monthPath)
			if err != nil {
				return fmt.Errorf("read rollout month %q: %w", monthPath, err)
			}
			for _, day := range days {
				if !validDatePart(day.Name(), 2) ||
					!validDateDirectory(year.Name(), month.Name(), day.Name()) {
					continue
				}
				dayRelative := filepath.Join(monthRelative, day.Name())
				if skipSymlinkDirectory(result, adapter.home, day, dayRelative) || !day.IsDir() {
					continue
				}
				dayPath := filepath.Join(monthPath, day.Name())
				if err := adapter.scanRolloutDirectory(result, dayPath, dayRelative, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func skipSymlinkDirectory(
	result *discoveryResult,
	root string,
	entry os.DirEntry,
	relative string,
) bool {
	if entry.Type()&os.ModeSymlink == 0 {
		return false
	}
	result.addSymlinkDiagnostic(root, filepath.ToSlash(relative))
	return true
}

func (adapter *Adapter) scanRolloutDirectory(
	result *discoveryResult,
	path string,
	relativeDir string,
	active bool,
) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read rollout directory %q: %w", path, err)
	}
	for _, entry := range entries {
		plain := plainName(entry.Name())
		compressed := compressedName(entry.Name())
		if !plain && !compressed {
			continue
		}
		relative := filepath.ToSlash(filepath.Join(relativeDir, entry.Name()))
		if entry.Type()&os.ModeSymlink != 0 {
			result.addSymlinkDiagnostic(adapter.home, relative)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat rollout entry %q: %w", filepath.Join(path, entry.Name()), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if plain {
			result.occurrences = append(result.occurrences, occurrence{
				relative: relative,
				active:   active,
			})
		} else {
			result.compressed = append(result.compressed, relative)
		}
	}
	return nil
}

func (result *discoveryResult) addSymlinkDiagnostic(root, relative string) {
	locator := sessionio.SourceLocator{
		Kind: sessionio.LocatorKindFile,
		File: &sessionio.FileLocator{Root: root, Path: filepath.ToSlash(relative)},
	}
	result.diagnostics = append(result.diagnostics, diagnostic(
		"codex_symlink_skipped",
		fmt.Sprintf("symlinked Codex rollout entry skipped: %s", filepath.ToSlash(relative)),
		&locator,
	))
}

func (adapter *Adapter) source(discovery discoveryResult) sessionio.Source {
	status := sessionio.SourceStatusAvailable
	if !discovery.activeRoot && !discovery.archiveRoot {
		status = sessionio.SourceStatusMissing
	}
	diagnostics := make([]sessionio.Diagnostic, 0, len(discovery.diagnostics)+3)
	if !discovery.activeRoot {
		diagnostics = append(diagnostics, diagnostic(
			"codex_root_missing",
			"Codex rollout root is absent: sessions",
			nil,
		))
	}
	if !discovery.archiveRoot {
		diagnostics = append(diagnostics, diagnostic(
			"codex_root_missing",
			"Codex rollout root is absent: archived_sessions",
			nil,
		))
	}
	diagnostics = append(diagnostics, discovery.diagnostics...)
	skipped := discovery.compressedOnlyCount()
	if skipped > 0 {
		suffix := "s"
		if skipped == 1 {
			suffix = ""
		}
		diagnostics = append(diagnostics, diagnostic(
			"codex_compressed_skipped",
			fmt.Sprintf("%d compressed rollout occurrence%s skipped", skipped, suffix),
			nil,
		))
	}
	return sessionio.Source{
		ID:           adapter.sourceID,
		Harness:      sessionio.HarnessCodex,
		Kind:         sessionio.SourceKindCanonical,
		Status:       status,
		Locator:      adapter.sourceLocator(),
		Capabilities: capabilities(),
		Diagnostics:  diagnostics,
	}
}

func (discovery discoveryResult) compressedOnlyCount() int {
	plain := make(map[string]struct{}, len(discovery.occurrences))
	for _, item := range discovery.occurrences {
		plain[item.relative] = struct{}{}
	}
	count := 0
	for _, compressed := range discovery.compressed {
		if _, found := plain[strings.TrimSuffix(compressed, ".zst")]; !found {
			count++
		}
	}
	return count
}

func (adapter *Adapter) sourceLocator() sessionio.SourceLocator {
	return sessionio.SourceLocator{
		Kind: sessionio.LocatorKindFile,
		File: &sessionio.FileLocator{Root: adapter.home, Path: "."},
	}
}

func plainName(name string) bool {
	_, valid := rolloutFilenameID(name, ".jsonl")
	return valid
}

func compressedName(name string) bool {
	_, valid := rolloutFilenameID(name, ".jsonl.zst")
	return valid
}

func rolloutFilenameID(name, suffix string) (string, bool) {
	core, found := strings.CutPrefix(name, "rollout-")
	if !found {
		return "", false
	}
	core, found = strings.CutSuffix(core, suffix)
	if !found || len(core) != len("2006-01-02T15-04-05")+1+36 {
		return "", false
	}
	const timestampLength = len("2006-01-02T15-04-05")
	if core[timestampLength] != '-' {
		return "", false
	}
	if !validRolloutTimestamp(core[:timestampLength]) {
		return "", false
	}
	id := core[timestampLength+1:]
	if !validUUID(id) {
		return "", false
	}
	return strings.ToLower(id), true
}

func validUUID(value string) bool {
	if len(value) != 36 ||
		value[8] != '-' ||
		value[13] != '-' ||
		value[18] != '-' ||
		value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16
}

func validDatePart(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validDateDirectory(year, month, day string) bool {
	yearValue := decimalValue(year)
	monthValue := decimalValue(month)
	dayValue := decimalValue(day)
	value := time.Date(
		yearValue,
		time.Month(monthValue),
		dayValue,
		0,
		0,
		0,
		0,
		time.UTC,
	)
	return value.Year() == yearValue &&
		int(value.Month()) == monthValue &&
		value.Day() == dayValue
}

func validRolloutTimestamp(value string) bool {
	if len(value) != len("2006-01-02T15-04-05") ||
		value[4] != '-' ||
		value[7] != '-' ||
		value[10] != 'T' ||
		value[13] != '-' ||
		value[16] != '-' {
		return false
	}
	for index, character := range value {
		if index == 4 || index == 7 || index == 10 || index == 13 || index == 16 {
			continue
		}
		if character < '0' || character > '9' {
			return false
		}
	}
	year := decimalValue(value[0:4])
	month := decimalValue(value[5:7])
	day := decimalValue(value[8:10])
	hour := decimalValue(value[11:13])
	minute := decimalValue(value[14:16])
	second := decimalValue(value[17:19])
	if hour > 23 || minute > 59 || second > 59 {
		return false
	}
	return validDateDirectory(
		fmt.Sprintf("%04d", year),
		fmt.Sprintf("%02d", month),
		fmt.Sprintf("%02d", day),
	)
}

func decimalValue(value string) int {
	result := 0
	for _, character := range value {
		result = result*10 + int(character-'0')
	}
	return result
}
