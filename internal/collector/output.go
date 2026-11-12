package collector

import (
	"bytes"
	"encoding/json"
	"io"
)

// writeEvent preserves the original JSON values, including large numbers.
// wrap is a literal object key, not a dotted path; empty means no wrapper.
func writeEvent(out io.Writer, event json.RawMessage, wrap string) error {
	var value any = event
	if wrap != "" {
		value = map[string]json.RawMessage{wrap: event}
	}
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	n, err := out.Write(line.Bytes())
	if err != nil {
		return err
	}
	if n != line.Len() {
		return io.ErrShortWrite
	}
	return nil
}
