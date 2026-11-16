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
	switch f {
	case JSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case TOON:
		b, err := toon.Marshal(r)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	default:
		WriteText(w, r)
		return nil
	}
}
