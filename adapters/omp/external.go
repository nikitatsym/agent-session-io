package omp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

var blobPattern = regexp.MustCompile(`^blob:sha256:([0-9a-f]{64})$`)
var artifactPattern = regexp.MustCompile(`(?:artifact://([0-9]+)|agent://([A-Za-z0-9_.-]+))`)

type externalPayload struct {
	reference string
	relative  string
	kind      string
	mediaType string
	hash      string
	info      os.FileInfo
	missing   bool
}

func (state *readState) indexExternal(data []byte, transcript string) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return walkStrings(value, func(text string) error {
		if strings.HasPrefix(text, "blob:") {
			match := blobPattern.FindStringSubmatch(text)
			if match == nil {
				return fmt.Errorf("malformed OMP blob reference %q", text)
			}
			return state.acquireExternal(text, "blobs/"+match[1], "blob", "application/octet-stream", match[1])
		}
		for _, match := range artifactPattern.FindAllStringSubmatch(text, -1) {
			ref := match[0]
			if _, found := state.external[ref]; found {
				continue
			}
			var candidates []string
			for _, dir := range artifactDirectories(transcript) {
				entries, err := os.ReadDir(filepath.Join(state.adapter.root, filepath.FromSlash(dir)))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				for _, entry := range entries {
					if match[2] != "" {
						if entry.Name() == match[2]+".md" || entry.Name() == match[2]+".json" {
							candidates = append(candidates, dir+"/"+entry.Name())
						}
					} else if strings.HasPrefix(entry.Name(), match[1]+".") && strings.HasSuffix(entry.Name(), ".log") {
						candidates = append(candidates, dir+"/"+entry.Name())
					}
				}
				if len(candidates) != 0 {
					break
				}
			}
			if match[2] != "" {
				if strings.Contains(match[2], "..") {
					return errors.New("malformed OMP agent output reference")
				}
				if len(candidates) == 0 {
					candidates = []string{strings.TrimSuffix(transcript, ".jsonl") + "/" + match[2] + ".md"}
				}
				for _, path := range candidates {
					media := "text/markdown"
					if strings.HasSuffix(path, ".json") {
						media = "application/json"
					}
					if err := state.acquireExternal(ref+":"+filepath.Base(path), path, "agent_output", media, ""); err != nil {
						return err
					}
				}
			} else {
				if len(candidates) > 1 {
					return fmt.Errorf("ambiguous OMP artifact reference %s", ref)
				}
				path := strings.TrimSuffix(transcript, ".jsonl") + "/" + match[1] + ".missing.log"
				if len(candidates) == 1 {
					path = candidates[0]
				}
				if err := state.acquireExternal(ref, path, "artifact", "text/plain", ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func artifactDirectories(transcript string) []string {
	result := []string{strings.TrimSuffix(transcript, ".jsonl")}
	for dir := filepath.ToSlash(filepath.Dir(transcript)); strings.Count(dir, "/") >= 2; dir = filepath.ToSlash(filepath.Dir(dir)) {
		result = append(result, dir)
	}
	return result
}

func walkStrings(value any, visit func(string) error) error {
	switch value := value.(type) {
	case string:
		return visit(value)
	case []any:
		for _, child := range value {
			if err := walkStrings(child, visit); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := walkStrings(value[key], visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (state *readState) acquireExternal(reference, relative, kind, mediaType, expectedHash string) error {
	if _, found := state.external[reference]; found {
		return nil
	}
	nativeReference := reference
	if kind == "agent_output" {
		nativeReference = strings.TrimSuffix(reference, ":"+filepath.Base(relative))
	}
	payload := &externalPayload{reference: nativeReference, relative: relative, kind: kind, mediaType: mediaType}
	state.external[reference] = payload
	state.externalOrder = append(state.externalOrder, reference)
	path := filepath.Join(state.adapter.root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		payload.missing = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat OMP external payload %s: %w", reference, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("OMP external payload %s is not a regular file", reference)
	}
	if state.adapter.maxRecordBytes != -1 && info.Size() > state.adapter.maxRecordBytes {
		return fmt.Errorf("OMP external payload %s exceeds configured record limit %d", reference, state.adapter.maxRecordBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	count, readErr := io.CopyN(hash, file, info.Size())
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if count != info.Size() {
		return errors.New("OMP external payload changed while acquiring")
	}
	payload.hash = hex.EncodeToString(hash.Sum(nil))
	if expectedHash != "" && payload.hash != expectedHash {
		return fmt.Errorf("OMP blob %s fails its SHA-256 content address", reference)
	}
	payload.info = info
	return nil
}

func (state *readState) limitations(data []byte) ([]sessionio.SourceLimitation, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	var limitations []sessionio.SourceLimitation
	seen := map[string]bool{}
	_ = walkStrings(value, func(text string) error {
		if strings.Contains(text, "[Session persistence truncated large content]") && !seen["truncated"] {
			seen["truncated"] = true
			limitations = append(limitations, sessionio.SourceLimitation{Kind: sessionio.LimitationKindUpstreamTruncation, Detail: "OMP truncated content before persistence"})
		}
		for _, reference := range state.externalOrder {
			nativeReference := state.external[reference].reference
			if !strings.Contains(text, nativeReference) || seen[reference] {
				continue
			}
			seen[reference] = true
			kind := sessionio.LimitationKindExternalPayload
			if state.external[reference].missing {
				kind = sessionio.LimitationKindMissingExternalPayload
			}
			limitations = append(limitations, sessionio.SourceLimitation{Kind: kind, Detail: nativeReference})
		}
		return nil
	})
	return limitations, nil
}

func (state *readState) nextExternal(ctx context.Context) (sessionio.ReadItem, error) {
	for state.externalPosition < len(state.externalOrder) {
		if err := ctx.Err(); err != nil {
			return sessionio.ReadItem{}, err
		}
		payload := state.external[state.externalOrder[state.externalPosition]]
		state.externalPosition++
		if payload.missing {
			continue
		}
		locator := state.adapter.locator(payload.relative)
		data, err := state.readExternal(payload)
		if err != nil {
			return sessionio.ReadItem{}, state.adapter.readerError("read", state.session.ID, locator, err)
		}
		locator.File.ByteRange = &sessionio.ByteRange{Start: 0, End: int64(len(data))}
		observation := sessionio.NativeObservation{ID: sessionio.ObservationID(sourceio.DerivedID("observation", string(state.session.ID), "external", payload.relative, payload.hash)), NativeKind: payload.kind, NativeKey: payload.reference, Locator: locator, Revision: sessionio.Revision{Kind: sessionio.RevisionKindFileSnapshot, Value: "sha256:" + payload.hash}, Representation: sessionio.NativeRepresentation{Capture: sessionio.CaptureKindByteExact, MediaType: payload.mediaType, Data: data}}
		item := sessionio.ReadItem{Session: state.session, Observation: observation}
		appendMarker(&item, payload.kind, payload.reference)
		return item, nil
	}
	return sessionio.ReadItem{}, io.EOF
}

func (state *readState) readExternal(payload *externalPayload) ([]byte, error) {
	path := filepath.Join(state.adapter.root, filepath.FromSlash(payload.relative))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(payload.info, info) || info.Size() != payload.info.Size() || info.ModTime() != payload.info.ModTime() {
		return nil, errors.New("OMP external payload changed before emission")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, payload.info.Size()))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(data)) != payload.info.Size() || digest(data) != payload.hash {
		return nil, errors.New("OMP external payload changed before emission")
	}
	return data, nil
}
