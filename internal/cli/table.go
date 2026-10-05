package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const (
	tableColumnGap  = 2
	minimumIDDigits = 12
	truncationTail  = "..."
)

// Only a terminal gets truncation: piped output must keep every cell whole.
func writeTable(writer io.Writer, headers []string, rows [][]string) error {
	for _, row := range rows {
		for column, cell := range row {
			row[column] = escapeControl(cell)
		}
	}
	if width, ok := terminalWidth(writer); ok {
		truncateLastColumn(headers, rows, width)
	}
	last := len(headers) - 1
	rendered := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderHeader(false).
		BorderColumn(false).
		BorderRow(false).
		Wrap(false).
		StyleFunc(func(row, column int) lipgloss.Style {
			style := lipgloss.NewStyle()
			if column < last {
				style = style.PaddingRight(tableColumnGap)
			}
			if row == table.HeaderRow {
				style = style.Bold(true)
			}
			return style
		}).
		Headers(headers...).
		Rows(rows...).
		Render()
	lines := strings.Split(rendered, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " ")
	}
	if _, err := lipgloss.Fprintln(writer, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	return nil
}

func terminalWidth(writer io.Writer) (int, bool) {
	file, ok := writer.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return 0, false
	}
	width, _, err := term.GetSize(file.Fd())
	if err != nil || width <= 0 {
		return 0, false
	}
	return width, true
}

func truncateLastColumn(headers []string, rows [][]string, width int) {
	last := len(headers) - 1
	used := 0
	for column := range last {
		widest := lipgloss.Width(headers[column])
		for _, row := range rows {
			widest = max(widest, lipgloss.Width(row[column]))
		}
		used += widest + tableColumnGap
	}
	available := width - used
	// No room for even one character before the tail: the row wraps instead.
	if available <= len(truncationTail) {
		return
	}
	headers[last] = ansi.Truncate(headers[last], available, truncationTail)
	for _, row := range rows {
		row[last] = ansi.Truncate(row[last], available, truncationTail)
	}
}

// Control characters would break the one-line row layout or drive the terminal.
func escapeControl(cell string) string {
	if strings.IndexFunc(cell, unicode.IsControl) < 0 {
		return cell
	}
	return strconv.Quote(cell)
}

// idAbbreviator picks per digest-form ID the shortest digest prefix (>= 12)
// that no other ID's full form or digest starts with, mirroring selectorMatches.
type idAbbreviator struct {
	short map[string]string
}

func newIDAbbreviator(ids []string) idAbbreviator {
	type key struct {
		value string
		owner string
	}
	keys := make([]key, 0, 2*len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		keys = append(keys, key{value: id, owner: id})
		if digest, ok := idDigest(id); ok {
			keys = append(keys, key{value: digest, owner: id})
		}
	}
	sort.Slice(keys, func(left, right int) bool {
		return keys[left].value < keys[right].value
	})
	short := make(map[string]string, len(seen))
	for index, candidate := range keys {
		if candidate.value == candidate.owner {
			continue
		}
		// In sorted order the nearest foreign key on each side shares the longest prefix.
		digits := min(minimumIDDigits, len(candidate.value))
		for _, step := range []int{-1, 1} {
			for neighbor := index + step; neighbor >= 0 && neighbor < len(keys); neighbor += step {
				if keys[neighbor].owner == candidate.owner {
					continue
				}
				digits = max(digits, commonPrefixLength(candidate.value, keys[neighbor].value)+1)
				break
			}
		}
		if digits <= len(candidate.value) {
			short[candidate.owner] = candidate.value[:digits]
		}
	}
	return idAbbreviator{short: short}
}

func (abbreviator idAbbreviator) abbreviate(id string) string {
	if short, found := abbreviator.short[id]; found {
		return short
	}
	return id
}

func idDigest(id string) (string, bool) {
	separator := strings.LastIndexByte(id, ':')
	return id[separator+1:], separator >= 0 && separator < len(id)-1
}

func commonPrefixLength(left, right string) int {
	length := 0
	for length < len(left) && length < len(right) && left[length] == right[length] {
		length++
	}
	return length
}

type timeStyle string

const (
	timeStyleAbsolute timeStyle = "absolute"
	timeStyleRelative timeStyle = "relative"
)

func parseTimeStyle(value string) (timeStyle, error) {
	switch style := timeStyle(value); style {
	case timeStyleAbsolute, timeStyleRelative:
		return style, nil
	default:
		return "", invalidUsage(fmt.Errorf(
			"invalid --time-style %q (expected absolute or relative)",
			value,
		))
	}
}

func formatHumanTime(value *time.Time, style timeStyle, now time.Time) string {
	if value == nil {
		return "-"
	}
	if style == timeStyleAbsolute {
		return value.Local().Format("2006-01-02 15:04")
	}
	age := now.Sub(*value)
	if age < 0 {
		return "in " + formatAge(-age)
	}
	if age < time.Minute {
		return "now"
	}
	return formatAge(age) + " ago"
}

func formatAge(age time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case age < time.Hour:
		return fmt.Sprintf("%dm", max(1, int(age/time.Minute)))
	case age < day:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	case age < 14*day:
		return fmt.Sprintf("%dd", int(age/day))
	case age < 60*day:
		return fmt.Sprintf("%dw", int(age/(7*day)))
	case age < 365*day:
		return fmt.Sprintf("%dmo", int(age/(30*day)))
	default:
		return fmt.Sprintf("%dy", int(age/(365*day)))
	}
}
