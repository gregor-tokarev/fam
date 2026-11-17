package output

import (
	"encoding/json"
	"io"

	"github.com/toon-format/toon-go"
)

// Format is the output format.
type Format string

// Formats.
const (
	Text Format = "text"
	JSON Format = "json"
	TOON Format = "toon"
)

// Write writes the result in the format.
func Write(w io.Writer, r *Run, f Format) error {
	return WriteValue(w, r, f, func() { WriteText(w, r) })
}

// WriteValue writes a command result with the shared structured encoders.
func WriteValue(w io.Writer, value any, f Format, plain func()) error {
	switch f {
	case JSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	case TOON:
		b, err := toon.Marshal(value)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	default:
		plain()
		return nil
	}
}
