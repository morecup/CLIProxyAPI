package helps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

// This is a byte-exact projection of the protected journal, not a final-report
// file and not an encrypted file mislabeled as native JSONL. Creation uses a
// private directory ACL on Windows (0700 alone does not restrict Windows ACLs).
func (s *ClaudeDesktopTranscriptStore) PrepareTaskOutput(scope string) (string, error) {
	journal, binding, err := s.path(scope)
	if err != nil {
		return "", err
	}
	lock := desktopContextLock(journal)
	lock.Lock()
	defer lock.Unlock()
	if _, err = readDesktopTranscript(journal, binding, nil); err != nil {
		return "", err
	}
	base := filepath.Join(filepath.Dir(filepath.Dir(journal)), "sdk-task-output")
	if os.MkdirAll(filepath.Dir(base), 0700) != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	for _, dir := range []string{base, filepath.Join(base, binding)} {
		if err = privateTaskOutputDirectory(dir); err != nil {
			return "", claudeprompt.ErrSDKSessionUnavailable
		}
	}
	path := filepath.Join(base, binding, "transcript.output")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR, 0600)
		if err == nil {
			err = verifyTaskProjection(f, journal, binding)
		}
	} else if err == nil {
		err = regularPrivateTaskOutput(f)
		if err == nil {
			_, err = readDesktopTranscriptContent(journal, binding, func(lines []byte) error {
				n, e := f.Write(lines)
				if e != nil || n != len(lines) {
					return claudeprompt.ErrSDKSessionUnavailable
				}
				return nil
			})
		}
		if err == nil {
			err = f.Sync()
		}
	}
	if f != nil {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	s.outputs.Store(scope, path)
	return path, nil
}

func verifyTaskProjection(f *os.File, journal, binding string) error {
	if err := regularPrivateTaskOutput(f); err != nil {
		return claudeprompt.ErrSDKSessionInvalid
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return claudeprompt.ErrSDKSessionUnavailable
	}
	_, err := readDesktopTranscriptContent(journal, binding, func(lines []byte) error {
		part := make([]byte, len(lines))
		if _, err := io.ReadFull(f, part); err != nil || !bytes.Equal(lines, part) {
			return claudeprompt.ErrSDKSessionInvalid
		}
		return nil
	})
	if err != nil {
		return err
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return claudeprompt.ErrSDKSessionInvalid
	}
	return nil
}

func regularTaskOutputPath(f *os.File) error {
	pathInfo, err := os.Lstat(f.Name())
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !pathInfo.Mode().IsRegular() || !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) {
		return os.ErrPermission
	}
	return nil
}

// The caller holds the journal's lock. A damaged projection is preserved as a
// diagnostic, never truncated, overwritten, silently healed or used as truth.
func (s *ClaudeDesktopTranscriptStore) openTaskProjectionLocked(scope, journal, binding string) (*os.File, error) {
	value, ok := s.outputs.Load(scope)
	if !ok {
		return nil, nil
	}
	f, err := os.OpenFile(value.(string), os.O_RDWR, 0600)
	if err != nil {
		return nil, claudeprompt.ErrSDKSessionUnavailable
	}
	if err = verifyTaskProjection(f, journal, binding); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *ClaudeDesktopTranscriptStore) ReadTaskOutput(scope string, limit int64) (string, error) {
	if limit < 1 || limit > 8<<20 {
		return "", claudeprompt.ErrSDKSessionInvalid
	}
	journal, binding, err := s.path(scope)
	if err != nil {
		return "", err
	}
	lock := desktopContextLock(journal)
	lock.Lock()
	defer lock.Unlock()
	f, err := s.openTaskProjectionLocked(scope, journal, binding)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	start := max(int64(0), info.Size()-limit)
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		_ = f.Close()
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || int64(len(data)) > limit {
		return "", claudeprompt.ErrSDKSessionUnavailable
	}
	// Node's UTF-8 decoder replaces a split leading sequence. Valid JSONL always
	// contains complete UTF-8; only the byte-bounded tail can split a code point.
	text := string(data)
	if !utf8.Valid(data) {
		var decoded strings.Builder
		for len(data) > 0 {
			r, n := utf8.DecodeRune(data)
			decoded.WriteRune(r)
			data = data[n:]
		}
		text = decoded.String()
	}
	if start > 0 {
		text = fmt.Sprintf("[%dKB of earlier output omitted]\n", (start+512)/1024) + text
	}
	return text, nil
}
