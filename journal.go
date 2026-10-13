package breakcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// journal records an applied mutation before the file is touched, so a run that
// is killed (even with SIGKILL) can be undone later by `breakcheck restore`.
type journal struct {
	File       string    `json:"file"`
	OrigSHA    string    `json:"orig_sha256"`
	MutatedSHA string    `json:"mutated_sha256"`
	Mode       uint32    `json:"mode"`
	PID        int       `json:"pid"`
	Started    time.Time `json:"started"`
}

const (
	journalName  = "journal.json"
	snapshotName = "original"
)

// ErrNoJournal means there is nothing to restore.
var ErrNoJournal = errors.New("no interrupted run to restore")

// writeJournal stores the original bytes first and the journal second, both
// fsynced, so a journal never points at a snapshot that is not on disk yet.
func writeJournal(state string, j *journal, orig []byte) error {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(state, snapshotName), orig, 0o600); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(state, journalName), b, 0o600)
}

func loadJournal(state string) (*journal, []byte, error) {
	b, err := os.ReadFile(filepath.Join(state, journalName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, nil, fmt.Errorf("unreadable journal %s: %w", filepath.Join(state, journalName), err)
	}
	snap, err := os.ReadFile(filepath.Join(state, snapshotName))
	if err != nil {
		return &j, nil, fmt.Errorf("the journal names %s but its snapshot is unreadable: %w", j.File, err)
	}
	if sum(snap) != j.OrigSHA {
		return &j, nil, fmt.Errorf("the snapshot does not match the journal's checksum; refusing to restore %s from it", j.File)
	}
	return &j, snap, nil
}

// Restore puts back the original bytes recorded in the journal under state and
// verifies them by checksum. If the file on disk is neither the original nor the
// mutated content, someone changed it after the mutation was applied, and Restore
// refuses to overwrite it unless force is set. It returns the restored path.
func Restore(state string, force bool) (string, error) {
	j, snap, err := loadJournal(state)
	if err != nil {
		return "", err
	}
	if j == nil {
		return "", ErrNoJournal
	}
	cur, err := os.ReadFile(j.File)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Gone: writing the original back cannot clobber anything.
	case err != nil:
		return j.File, err
	case sum(cur) == j.OrigSHA:
		return j.File, clearJournal(state) // already original
	case sum(cur) != j.MutatedSHA && !force:
		return j.File, fmt.Errorf("%s changed after the mutation was applied (it is neither the original nor the mutated bytes), so it was not overwritten; the original is kept in %s, and `breakcheck restore --force` puts it back", j.File, filepath.Join(state, snapshotName))
	}
	if err := writeAtomic(j.File, snap, os.FileMode(j.Mode)); err != nil {
		return j.File, err
	}
	if got, err := os.ReadFile(j.File); err != nil || sum(got) != j.OrigSHA {
		return j.File, fmt.Errorf("wrote %s but it does not read back as the original bytes; the snapshot is kept in %s", j.File, state)
	}
	return j.File, clearJournal(state)
}

func clearJournal(state string) error {
	if err := os.Remove(filepath.Join(state, journalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(state, snapshotName))
	_ = os.Remove(state) // only succeeds when empty
	return nil
}

// writeAtomic replaces path with data via a temp file in the same directory and
// a rename, so a crash leaves either the old bytes or the new ones, never half.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".breakcheck-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
