package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrReplay means an OIDC token id was already used and is not yet expired.
var ErrReplay = errors.New("oidc token already used")

// RecordJTI stores an OIDC token id until expiresAt. It is atomic: the
// primary key decides, so of two concurrent requests with the same jti
// exactly one succeeds and the other gets ErrReplay. A leftover row whose
// expiry has passed (not purged yet) is overwritten rather than treated
// as a replay. Rows survive restarts.
func (s *Store) RecordJTI(jti string, expiresAt, now time.Time) error {
	res, err := s.db.ExecContext(ctx(), `
		INSERT INTO oidc_jti (jti, expires_at) VALUES (?, ?)
		ON CONFLICT(jti) DO UPDATE SET expires_at = excluded.expires_at
		WHERE oidc_jti.expires_at <= ?`,
		jti, expiresAt.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("record jti: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record jti: %w", err)
	}
	if n == 0 {
		return ErrReplay
	}
	return nil
}

// PurgeExpiredJTI deletes token ids whose expiry is not after now and
// returns how many were removed.
func (s *Store) PurgeExpiredJTI(now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx(), `DELETE FROM oidc_jti WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("purge jti: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge jti: %w", err)
	}
	return n, nil
}

// ReleaseJTI forgets a token id, used when the request that recorded it
// did not start a deploy after all.
func (s *Store) ReleaseJTI(jti string) error {
	_, err := s.db.ExecContext(ctx(), `DELETE FROM oidc_jti WHERE jti = ?`, jti)
	if err != nil {
		return fmt.Errorf("release jti: %w", err)
	}
	return nil
}
