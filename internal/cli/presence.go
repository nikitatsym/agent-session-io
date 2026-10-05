package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func writePresence(
	writer io.Writer,
	producer sessionio.Producer,
	format outputFormat,
	snapshot sessionio.PresenceSnapshot,
	ids idAbbreviator,
) error {
	switch format {
	case formatJSON:
		return sessionio.WritePresenceJSON(writer, producer, snapshot)
	case formatNDJSON:
		encoder, err := sessionio.NewPresenceNDJSONEncoder(writer, producer)
		if err != nil {
			return err
		}
		return encoder.Encode(snapshot)
	default:
		return writePresenceHuman(writer, snapshot, ids)
	}
}

func writePresenceHuman(
	writer io.Writer,
	snapshot sessionio.PresenceSnapshot,
	ids idAbbreviator,
) error {
	var rows [][]string
	for _, match := range snapshot.Matches {
		state := "likely open"
		if match.Certainty == sessionio.PresenceCertaintyExact {
			state = "open"
		}
		selector := fmt.Sprintf("ambiguous(%d)", len(match.Occurrences))
		title := ""
		if match.Selection.Status == sessionio.PresenceSelectionResolved {
			selector = ids.abbreviate(string(match.Selection.SessionID))
			title = selectedPresenceTitle(match)
		}
		rows = append(rows, []string{
			state,
			string(match.Harness),
			selector,
			match.NativeSessionID,
			presencePIDs(match.Processes),
			presenceEvidenceKinds(match.Evidence),
			oneLine(title),
		})
		if match.Selection.Status == sessionio.PresenceSelectionAmbiguous {
			for _, occurrence := range match.Occurrences {
				rows = append(rows, []string{
					"candidate",
					string(match.Harness),
					ids.abbreviate(string(occurrence.Session.ID)),
					match.NativeSessionID,
					"-",
					"-",
					oneLine(occurrence.Session.Title),
				})
			}
		}
	}
	for _, unmatched := range snapshot.UnmatchedProcesses {
		nativeIDs := "-"
		if len(unmatched.ClaimedNativeIDs) > 0 {
			nativeIDs = strings.Join(unmatched.ClaimedNativeIDs, ",")
		}
		rows = append(rows, []string{
			"unmatched process",
			string(unmatched.Harness),
			"-",
			nativeIDs,
			strconv.FormatUint(unmatched.Process.PID, 10),
			presenceEvidenceKinds(unmatched.Evidence),
			string(unmatched.Reason),
		})
	}
	return writeTable(
		writer,
		[]string{"STATE", "HARNESS", "SESSION", "NATIVE ID", "PROCESSES", "EVIDENCE", "TITLE"},
		rows,
	)
}

func writePresenceDiagnostics(
	writer io.Writer,
	snapshot sessionio.PresenceSnapshot,
) error {
	for _, provider := range snapshot.Providers {
		if provider.Support != sessionio.PresenceSupportSupported {
			if _, err := fmt.Fprintf(
				writer,
				"presence %s %s: %s%s\n",
				provider.Harness,
				provider.Support,
				presenceReason(provider.Reason),
				presenceDetail(provider.Detail),
			); err != nil {
				return fmt.Errorf("write presence provider diagnostic: %w", err)
			}
		}
		for _, capability := range provider.Capabilities {
			if capability.Support == sessionio.PresenceSupportSupported {
				continue
			}
			if _, err := fmt.Fprintf(
				writer,
				"presence %s %s %s: %s%s\n",
				provider.Harness,
				capability.Capability,
				capability.Support,
				presenceReason(capability.Reason),
				presenceDetail(capability.Detail),
			); err != nil {
				return fmt.Errorf("write presence capability diagnostic: %w", err)
			}
		}
	}
	return nil
}

func selectedPresenceTitle(match sessionio.PresenceMatch) string {
	for _, occurrence := range match.Occurrences {
		if occurrence.Session.ID == match.Selection.SessionID {
			return occurrence.Session.Title
		}
	}
	return ""
}

func presencePIDs(processes []sessionio.ProcessInstance) string {
	values := make([]string, len(processes))
	for index, process := range processes {
		values[index] = strconv.FormatUint(process.PID, 10)
	}
	return strings.Join(values, ",")
}

func presenceEvidenceKinds(evidence []sessionio.PresenceEvidence) string {
	kinds := make([]string, 0, len(evidence))
	for _, item := range evidence {
		kind := string(item.Kind)
		if len(kinds) == 0 || kinds[len(kinds)-1] != kind {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return "-"
	}
	return strings.Join(kinds, ",")
}

func presenceReason(reason *sessionio.PresenceReason) string {
	if reason == nil {
		return "unknown"
	}
	return string(*reason)
}

func presenceDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + oneLine(detail)
}
