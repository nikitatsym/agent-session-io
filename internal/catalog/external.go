package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	sessionio "github.com/nikitatsym/agent-session-io"
)

// EncodeExternalSnapshot preserves every acquired unit outside the canonical container.
func EncodeExternalSnapshot(items []sessionio.ReadItem, canonical sessionio.SourceLocator) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, item := range items {
		locator := item.Observation.Locator
		if canonical.File != nil && locator.File != nil && canonical.File.Root == locator.File.Root && canonical.File.Path == locator.File.Path {
			continue
		}
		if err := validateExternalItem(item); err != nil {
			return nil, err
		}
		if err := encoder.Encode(item); err != nil {
			return nil, fmt.Errorf("encode external observation: %w", err)
		}
	}
	return buffer.Bytes(), nil
}

// DecodeExternalSnapshot restores evidence without consulting the original source.
func DecodeExternalSnapshot(data []byte) ([]sessionio.ReadItem, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var items []sessionio.ReadItem
	for {
		var item sessionio.ReadItem
		if err := decoder.Decode(&item); err == io.EOF {
			return items, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode external observation: %w", err)
		}
		if err := validateExternalItem(item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
}

func validateExternalItem(item sessionio.ReadItem) error {
	encoder, err := sessionio.NewNDJSONEncoder(io.Discard, sessionio.Producer{Name: "sessionio", Version: "1"})
	if err != nil {
		return err
	}
	return encoder.Encode(sessionio.Record{Kind: sessionio.RecordKindReadItem, ReadItem: &item})
}
