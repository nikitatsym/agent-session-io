package sessionio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PresenceSchema identifies the runtime-presence machine-output schema.
const PresenceSchema = "sessionio.presence/v1"

type presenceDocument struct {
	Schema   string           `json:"schema"`
	Producer Producer         `json:"producer"`
	Snapshot PresenceSnapshot `json:"snapshot"`
}

// WritePresenceJSON writes one validated presence snapshot followed by a newline.
func WritePresenceJSON(writer io.Writer, producer Producer, snapshot PresenceSnapshot) error {
	if nilInterface(writer) {
		return errors.New("sessionio: presence JSON writer must not be nil")
	}
	if err := validateProducer(producer); err != nil {
		return err
	}
	if err := ValidatePresenceSnapshot(snapshot); err != nil {
		return err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(presenceDocument{Schema: PresenceSchema, Producer: producer, Snapshot: snapshot}); err != nil {
		return fmt.Errorf("sessionio: write presence JSON document: %w", err)
	}
	return nil
}

// PresenceNDJSONEncoder writes independently decodable presence snapshots.
type PresenceNDJSONEncoder struct {
	producer Producer
	encoder  *json.Encoder
}

// NewPresenceNDJSONEncoder creates a validated presence NDJSON encoder.
func NewPresenceNDJSONEncoder(writer io.Writer, producer Producer) (*PresenceNDJSONEncoder, error) {
	if nilInterface(writer) {
		return nil, errors.New("sessionio: presence NDJSON writer must not be nil")
	}
	if err := validateProducer(producer); err != nil {
		return nil, err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return &PresenceNDJSONEncoder{producer: producer, encoder: encoder}, nil
}

// Encode validates and writes one self-describing presence snapshot.
func (encoder *PresenceNDJSONEncoder) Encode(snapshot PresenceSnapshot) error {
	if encoder == nil || encoder.encoder == nil {
		return errors.New("sessionio: presence NDJSON encoder must not be nil")
	}
	if err := ValidatePresenceSnapshot(snapshot); err != nil {
		return err
	}
	if err := encoder.encoder.Encode(presenceDocument{Schema: PresenceSchema, Producer: encoder.producer, Snapshot: snapshot}); err != nil {
		return fmt.Errorf("sessionio: write presence NDJSON snapshot: %w", err)
	}
	return nil
}
