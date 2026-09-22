package executor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var claudeDesktopDurableNamespaces = [...]string{
	"desktop-session-records",
	"sdk-session-aliases",
	"sdk-sessions",
	"sdk-native-content",
	"sdk-transcripts",
	"sdk-remote-transcripts",
	"sdk-agent-tasks",
	"owned-context",
}

// migrateClaudeDesktopDurableState imports only state that belongs to the
// account across runtime revisions. PreviousRevision is intentionally first:
// after promotion it contains the session catalog that existed before drift.
// Existing durable files always win, so retries and later rollbacks cannot
// replace an already accepted record or transcript.
func migrateClaudeDesktopDurableState(accountDirectory, durableStatePath string, binding claudeDesktopRuntimeBinding) error {
	accountDirectory = strings.TrimSpace(accountDirectory)
	durableStatePath = strings.TrimSpace(durableStatePath)
	if accountDirectory == "" || durableStatePath == "" {
		return errors.New("account durable state path is unavailable")
	}
	if errMkdir := os.MkdirAll(durableStatePath, 0o700); errMkdir != nil {
		return fmt.Errorf("create durable state directory: %w", errMkdir)
	}
	seen := make(map[string]bool, 2)
	for _, revision := range []string{binding.PreviousRevision, binding.ApprovedRevision} {
		revision = strings.TrimSpace(revision)
		if revision == "" || seen[revision] {
			continue
		}
		if !validClaudeDesktopRuntimeRevision(revision) {
			return errors.New("durable state source revision is invalid")
		}
		seen[revision] = true
		sourceRoot := filepath.Join(accountDirectory, "instances", claudeDesktopRevisionInstanceID(revision))
		for _, namespace := range claudeDesktopDurableNamespaces {
			if errCopy := copyMissingClaudeDesktopDurableNamespace(
				filepath.Join(sourceRoot, namespace),
				filepath.Join(durableStatePath, namespace),
			); errCopy != nil {
				return fmt.Errorf("import %s from revision %s: %w", namespace, claudeDesktopRevisionInstanceID(revision), errCopy)
			}
		}
	}
	return nil
}

func copyMissingClaudeDesktopDurableNamespace(source, destination string) error {
	info, errInfo := os.Lstat(source)
	if errors.Is(errInfo, fs.ErrNotExist) {
		return nil
	}
	if errInfo != nil {
		return errInfo
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source namespace is not a directory")
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, errWalk error) error {
		if errWalk != nil {
			return errWalk
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return errors.New("source namespace contains a symbolic link")
		}
		relative, errRelative := filepath.Rel(source, path)
		if errRelative != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("source namespace escaped its root")
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("source namespace contains a non-regular file")
		}
		return copyMissingClaudeDesktopDurableFile(path, target)
	})
}

func copyMissingClaudeDesktopDurableFile(source, destination string) error {
	if info, errInfo := os.Lstat(destination); errInfo == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("durable destination is not a regular file")
		}
		return nil
	} else if !errors.Is(errInfo, fs.ErrNotExist) {
		return errInfo
	}
	if errMkdir := os.MkdirAll(filepath.Dir(destination), 0o700); errMkdir != nil {
		return errMkdir
	}
	input, errOpen := os.Open(source)
	if errOpen != nil {
		return errOpen
	}
	defer input.Close()
	output, errCreate := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(errCreate, fs.ErrExist) {
		return nil
	}
	if errCreate != nil {
		return errCreate
	}
	complete := false
	defer func() {
		_ = output.Close()
		if !complete {
			_ = os.Remove(destination)
		}
	}()
	if _, errCopy := io.Copy(output, input); errCopy != nil {
		return errCopy
	}
	if errSync := output.Sync(); errSync != nil {
		return errSync
	}
	if errClose := output.Close(); errClose != nil {
		return errClose
	}
	complete = true
	return nil
}
