package fsutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrTrailingJSON reports unexpected data after the first JSON value.
var ErrTrailingJSON = errors.New("unexpected trailing JSON data")

// DecodeStrictJSON decodes exactly one JSON value with unknown fields rejected.
func DecodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrTrailingJSON
	}
	return nil
}

// ReadStrictJSON reads a bounded regular non-symlink JSON file and decodes it strictly.
func ReadStrictJSON(path string, maxBytes int64, target any) error {
	data, _, err := ReadBoundedRegular(path, maxBytes)
	if err != nil {
		return err
	}
	if err := DecodeStrictJSON(data, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// MarshalJSONIndent encodes value as 2-space indented JSON with a trailing newline.
func MarshalJSONIndent(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteJSONAtomic marshals value with two-space indentation and publishes it atomically.
func WriteJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := MarshalJSONIndent(value)
	if err != nil {
		return err
	}
	return AtomicWrite(path, data, mode)
}
