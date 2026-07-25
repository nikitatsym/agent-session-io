// Package config loads the versioned strict TOML configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Schema is the only configuration contract version sessionio accepts.
const Schema = "sessionio.config/v1"

// DefaultSchemaName is used when [search] omits schema_name.
const DefaultSchemaName = "sessionio"

// BackendPostgres is the only supported search backend.
const BackendPostgres = "postgres"

// SchemaNamePattern also guarantees SQL identifier safety for the catalog.
const SchemaNamePattern = `^[a-z_][a-z0-9_]{0,62}$`

var schemaNameExpression = regexp.MustCompile(SchemaNamePattern)

type Config struct {
	Schema string `toml:"schema"`
	Search Search `toml:"search"`
}

type Search struct {
	Backend    string `toml:"backend"`
	DSNEnv     string `toml:"dsn_env"`
	DSN        string `toml:"dsn"`
	SchemaName string `toml:"schema_name"`
}

// Error reports invalid configuration. It never carries a DSN value.
type Error struct {
	Path        string
	Field       string
	Message     string
	Remediation string
	cause       error
}

func (configError *Error) Error() string {
	if configError.Path == "" {
		return configError.Message
	}
	return fmt.Sprintf("%s: %s", configError.Path, configError.Message)
}

func (configError *Error) Unwrap() error {
	return configError.cause
}

// Details describes the failure for machine output without secrets.
func (configError *Error) Details() map[string]any {
	details := map[string]any{}
	if configError.Path != "" {
		details["path"] = configError.Path
	}
	if configError.Field != "" {
		details["field"] = configError.Field
	}
	return details
}

// DefaultPath is the configuration file used when --config is absent.
func DefaultPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	return filepath.Join(directory, "sessionio", "config.toml"), nil
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &Error{
				Path:    path,
				Message: "configuration file does not exist",
				Remediation: "create the file or pass --config with" +
					" an existing path",
				cause: err,
			}
		}
		return nil, &Error{
			Path:        path,
			Message:     fmt.Sprintf("configuration file is unreadable: %v", err),
			Remediation: "grant read access to the configuration file",
			cause:       err,
		}
	}
	return Parse(path, data)
}

// Parse validates configuration bytes that were read from path.
func Parse(path string, data []byte) (*Config, error) {
	var parsed Config
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, decodeError(path, err)
	}
	if err := parsed.validate(path); err != nil {
		return nil, err
	}
	return &parsed, nil
}

func decodeError(path string, err error) error {
	var missing *toml.StrictMissingError
	if errors.As(err, &missing) {
		fields := make([]string, 0, len(missing.Errors))
		for index := range missing.Errors {
			fields = append(
				fields,
				strings.Join(missing.Errors[index].Key(), "."),
			)
		}
		return &Error{
			Path:  path,
			Field: fields[0],
			Message: fmt.Sprintf(
				"unknown configuration field %s",
				strings.Join(fields, ", "),
			),
			Remediation: "remove the field; sessionio models only" +
				" schema and [search] in this revision",
			cause: err,
		}
	}
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		row, column := decode.Position()
		return &Error{
			Path: path,
			Message: fmt.Sprintf(
				"invalid TOML at line %d column %d: %s",
				row,
				column,
				decode.Error(),
			),
			Remediation: "fix the TOML syntax reported above",
			cause:       err,
		}
	}
	return &Error{
		Path:        path,
		Message:     fmt.Sprintf("decode configuration: %v", err),
		Remediation: "fix the configuration file",
		cause:       err,
	}
}

func (parsed *Config) validate(path string) error {
	if parsed.Schema != Schema {
		return &Error{
			Path:  path,
			Field: "schema",
			Message: fmt.Sprintf(
				"schema %q is unsupported, expected %q",
				parsed.Schema,
				Schema,
			),
			Remediation: fmt.Sprintf("set schema = %q", Schema),
		}
	}
	if parsed.Search.Backend != BackendPostgres {
		return &Error{
			Path:  path,
			Field: "search.backend",
			Message: fmt.Sprintf(
				"search.backend %q is unsupported, expected %q",
				parsed.Search.Backend,
				BackendPostgres,
			),
			Remediation: fmt.Sprintf(
				"set backend = %q under [search]",
				BackendPostgres,
			),
		}
	}
	namedEnvironment := parsed.Search.DSNEnv != ""
	literalDSN := parsed.Search.DSN != ""
	if namedEnvironment == literalDSN {
		given := "neither was given"
		if literalDSN {
			given = "both were given"
		}
		return &Error{
			Path:  path,
			Field: "search.dsn_env",
			Message: "[search] requires exactly one of dsn_env and dsn, " +
				given,
			Remediation: "set dsn_env to an environment variable name" +
				" or dsn to a literal URL, not both",
		}
	}
	if parsed.Search.SchemaName == "" {
		parsed.Search.SchemaName = DefaultSchemaName
	}
	if !schemaNameExpression.MatchString(parsed.Search.SchemaName) {
		return &Error{
			Path:  path,
			Field: "search.schema_name",
			Message: fmt.Sprintf(
				"search.schema_name %q is invalid, expected %s",
				parsed.Search.SchemaName,
				SchemaNamePattern,
			),
			Remediation: "use a lower-case PostgreSQL identifier such as" +
				" sessionio",
		}
	}
	return nil
}
