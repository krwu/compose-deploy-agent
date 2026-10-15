package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_RecordJTISurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	now := time.Unix(1_800_000_000, 0)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	err = st.RecordJTI("jti-1", now.Add(5*time.Minute), now)
	if err != nil {
		t.Fatalf("first record: %v", err)
	}
	err = st.RecordJTI("jti-1", now.Add(5*time.Minute), now)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("second record: want ErrReplay, got %v", err)
	}
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	err = st.RecordJTI("jti-1", now.Add(5*time.Minute), now.Add(time.Minute))
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("after restart: want ErrReplay, got %v", err)
	}
}

func TestStore_JTIExpiryAndPurge(t *testing.T) {
	st := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	err := st.RecordJTI("old", now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("record old: %v", err)
	}
	err = st.RecordJTI("live", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("record live: %v", err)
	}
	later := now.Add(2 * time.Minute)
	err = st.RecordJTI("old", later.Add(time.Minute), later)
	if err != nil {
		t.Fatalf("expired leftover must not count as replay: %v", err)
	}
	n, err := st.PurgeExpiredJTI(later.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purge removed %d rows, want 1", n)
	}
	err = st.RecordJTI("live", now.Add(time.Hour), later)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("live jti must still be recorded, got %v", err)
	}
}

func TestStore_ReleaseJTI(t *testing.T) {
	st := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	err := st.RecordJTI("jti-1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	err = st.ReleaseJTI("jti-1")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	err = st.RecordJTI("jti-1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("record after release: %v", err)
	}
}
