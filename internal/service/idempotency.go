package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// ErrIdempotencyMismatch: the key was already used for a different request.
var ErrIdempotencyMismatch = errors.New("idempotency key reused for a different request")

// StoredResponse is a response recorded under an Idempotency-Key.
type StoredResponse struct {
	Status int
	Body   json.RawMessage
}

// RequestHash fingerprints an action request (name + raw body).
func RequestHash(action string, body []byte) string {
	sum := sha256.Sum256(append([]byte(action+"\n"), body...))
	return hex.EncodeToString(sum[:])
}

// Responder turns an action outcome (result or error) into the HTTP status and JSON body that is returned and stored.
type Responder func(res Result, err error) (int, json.RawMessage)

// DispatchIdempotent runs an action at most once per (user, Idempotency-Key) (F1.8). The key is claimed inside the
// action's own transaction, so the effects and the stored response commit together. A concurrent duplicate blocks on
// the key's row until the first commits, then replays the stored response. When the action is rejected (rule
// violation, forbidden, unknown action) nothing was written; that answer is stored in a short follow-up transaction
// so a retry gets the same response too. Returns replayed=true for a replay.
func (s *Service) DispatchIdempotent(ctx context.Context, viewer *Viewer, key, requestHash, name string, args []json.RawMessage, wantID string,
	respond Responder) (StoredResponse, bool, error) {
	userID := ""
	if viewer != nil {
		userID = viewer.UserID
	}
	get := func(q *dbq.Queries) (StoredResponse, bool, error) {
		prev, err := q.GetIdempotencyKey(ctx, dbq.GetIdempotencyKeyParams{UserID: userID, Key: key})
		if err != nil {
			return StoredResponse{}, false, err
		}
		if prev.RequestHash != requestHash {
			return StoredResponse{}, false, ErrIdempotencyMismatch
		}
		return StoredResponse{Status: prev.Status, Body: json.RawMessage(prev.Response)}, true, nil
	}
	if out, ok, err := get(s.q); err == nil || !db.IsNotFound(err) {
		return out, ok, err
	}
	// A KYC-backed review is decided by the KYC module in its own transaction. kyc.Decide refuses a second
	// decision, so a retry gets a rule violation instead of a double decision; the key is not stored.
	if res, handled, err := s.reviewViaKYC(ctx, viewer, name, args); handled {
		if err != nil && !errors.Is(err, domain.ErrForbidden) {
			if _, isRule := IsUserError(err); !isRule {
				return StoredResponse{}, false, err
			}
			res.Version, _ = s.Version(ctx)
		}
		status, body := respond(res, err)
		return StoredResponse{Status: status, Body: body}, false, nil
	}

	var out StoredResponse
	var replayed bool
	_, err := s.run(ctx, func(t *txn) error {
		n, err := t.q.ClaimIdempotencyKey(t.ctx, dbq.ClaimIdempotencyKeyParams{UserID: userID, Key: key, Action: name, RequestHash: requestHash,
			Status: 0, Response: "null"})
		if !t.check(err) {
			return nil
		}
		if n == 0 { // a concurrent request with the same key committed first
			out, replayed, err = get(t.q)
			if errors.Is(err, ErrIdempotencyMismatch) {
				return err
			}
			t.check(err)
			return nil
		}
		value, err := t.dispatch(viewer, name, args, wantID)
		if err != nil || t.err != nil {
			return err
		}
		ch, err := t.finish()
		if !t.check(err) {
			return nil
		}
		res := Result{Value: value, Version: ch.Version, Change: ch}
		if res.Version == 0 {
			res.Version, err = t.q.CurrentVersion(t.ctx)
			t.check(err)
		}
		status, body := respond(res, nil)
		out = StoredResponse{Status: status, Body: body}
		t.check(t.q.CompleteIdempotencyKey(t.ctx, dbq.CompleteIdempotencyKeyParams{UserID: userID, Key: key, Status: status, Response: string(body)}))
		return nil
	})
	if err == nil {
		return out, replayed, nil
	}
	if errors.Is(err, ErrIdempotencyMismatch) {
		return StoredResponse{}, false, err
	}
	if _, isRule := IsUserError(err); !isRule && !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrUnknownAction) {
		return StoredResponse{}, false, err
	}
	version, verr := s.Version(ctx)
	if verr != nil {
		return StoredResponse{}, false, verr
	}
	status, body := respond(Result{Version: version}, err)
	out = StoredResponse{Status: status, Body: body}
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		n, err := q.ClaimIdempotencyKey(ctx, dbq.ClaimIdempotencyKeyParams{UserID: userID, Key: key, Action: name, RequestHash: requestHash, Status: status, Response: string(body)})
		if err != nil || n > 0 {
			return err
		}
		out, replayed, err = get(q)
		return err
	})
	return out, replayed, err
}
