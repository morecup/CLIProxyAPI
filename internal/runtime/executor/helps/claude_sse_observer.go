package helps

import (
	"bufio"
	"bytes"
	"io"
)

// ReadClaudeSSEWithObserver preserves the response bytes while observing each
// complete line before reading the next one. A buffered downstream response
// must not move the SDK's first-assistant observation to upstream EOF.
func ReadClaudeSSEWithObserver(reader io.Reader, observe func([]byte) error) ([]byte, error) {
	if observe == nil {
		return io.ReadAll(reader)
	}
	buffered := bufio.NewReader(reader)
	var response bytes.Buffer
	for {
		line, errRead := buffered.ReadBytes('\n')
		_, _ = response.Write(line)
		if len(line) > 0 && (errRead == nil || errRead == io.EOF) {
			if errObserve := observe(bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'})); errObserve != nil {
				return response.Bytes(), errObserve
			}
		}
		if errRead != nil {
			if errRead == io.EOF {
				return response.Bytes(), nil
			}
			return response.Bytes(), errRead
		}
	}
}
