package sessionio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ReaderSchema identifies the initial reader machine-output schema.
const ReaderSchema = "sessionio.reader/v1"

// Producer identifies the program that emitted machine output.
type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// RecordKind selects the active Record variant.
type RecordKind string

const (
	RecordKindSource     RecordKind = "source"
	RecordKindSession    RecordKind = "session"
	RecordKindReadItem   RecordKind = "read_item"
	RecordKindDiagnostic RecordKind = "diagnostic"
)

// Record is one validated reader machine-output record.
type Record struct {
	Kind       RecordKind  `json:"kind"`
	Source     *Source     `json:"source,omitempty"`
	Session    *SessionRef `json:"session,omitempty"`
	ReadItem   *ReadItem   `json:"read_item,omitempty"`
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
}

type jsonDocument struct {
	Schema   string   `json:"schema"`
	Producer Producer `json:"producer"`
	Records  []Record `json:"records"`
}

type ndjsonRecord struct {
	Schema   string   `json:"schema"`
	Producer Producer `json:"producer"`
	Record   Record   `json:"record"`
}

// WriteJSON writes one validated reader document followed by a newline.
func WriteJSON(writer io.Writer, producer Producer, records []Record) error {
	if nilInterface(writer) {
		return errors.New("sessionio: JSON writer must not be nil")
	}
	if err := validateProducer(producer); err != nil {
		return err
	}
	for index, record := range records {
		if err := validateRecord(fmt.Sprintf("records[%d]", index), record); err != nil {
			return err
		}
	}

	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(jsonDocument{
		Schema:   ReaderSchema,
		Producer: producer,
		Records:  records,
	}); err != nil {
		return fmt.Errorf("sessionio: write JSON document: %w", err)
	}
	return nil
}

// NDJSONEncoder writes independently decodable reader records.
type NDJSONEncoder struct {
	producer Producer
	encoder  *json.Encoder
}

// NewNDJSONEncoder creates a validated NDJSON encoder.
func NewNDJSONEncoder(writer io.Writer, producer Producer) (*NDJSONEncoder, error) {
	if nilInterface(writer) {
		return nil, errors.New("sessionio: NDJSON writer must not be nil")
	}
	if err := validateProducer(producer); err != nil {
		return nil, err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return &NDJSONEncoder{
		producer: producer,
		encoder:  encoder,
	}, nil
}

// Encode validates and writes one self-describing NDJSON record.
func (encoder *NDJSONEncoder) Encode(record Record) error {
	if encoder == nil || encoder.encoder == nil {
		return errors.New("sessionio: NDJSON encoder must not be nil")
	}
	if err := validateRecord("record", record); err != nil {
		return err
	}
	if err := encoder.encoder.Encode(ndjsonRecord{
		Schema:   ReaderSchema,
		Producer: encoder.producer,
		Record:   record,
	}); err != nil {
		return fmt.Errorf("sessionio: write NDJSON record: %w", err)
	}
	return nil
}

func validateProducer(producer Producer) error {
	if producer.Name == "" {
		return invalid("producer.name", "must not be empty")
	}
	if producer.Version == "" {
		return invalid("producer.version", "must not be empty")
	}
	return nil
}

func validateRecord(path string, record Record) error {
	variants := 0
	if record.Source != nil {
		variants++
	}
	if record.Session != nil {
		variants++
	}
	if record.ReadItem != nil {
		variants++
	}
	if record.Diagnostic != nil {
		variants++
	}
	if variants != 1 {
		return invalid(path, "must contain exactly one record variant")
	}

	switch record.Kind {
	case RecordKindSource:
		if record.Source == nil {
			return invalid(path, "kind %q requires source variant", record.Kind)
		}
		return validateSource(path+".source", *record.Source)
	case RecordKindSession:
		if record.Session == nil {
			return invalid(path, "kind %q requires session variant", record.Kind)
		}
		return validateSessionRef(path+".session", *record.Session)
	case RecordKindReadItem:
		if record.ReadItem == nil {
			return invalid(path, "kind %q requires read_item variant", record.Kind)
		}
		return validateReadItem(path+".read_item", *record.ReadItem)
	case RecordKindDiagnostic:
		if record.Diagnostic == nil {
			return invalid(path, "kind %q requires diagnostic variant", record.Kind)
		}
		return validateDiagnostic(path+".diagnostic", *record.Diagnostic)
	default:
		return invalid(path+".kind", "unsupported value %q", record.Kind)
	}
}
