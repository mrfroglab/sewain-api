package service_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// The functions the auth module builds on (OTP login creates accounts inside its own transaction).
func TestUserFunctionsForAuth(t *testing.T) {
	e := newEnv(t)
	u, err := e.svc.FindUserByPhone(e.ctx, "+62 812-2734-0011")
	if err != nil || u == nil || u.ID != "u-raka" {
		t.Fatalf("find by +62 phone: %v %v", u, err)
	}
	if u, err := e.svc.FindUserByPhone(e.ctx, "0899"); err != nil || u != nil {
		t.Fatalf("unknown phone: %v %v", u, err)
	}
	auth, err := e.svc.GetUserAuth(e.ctx, "u-admin")
	if err != nil || auth.Role != "admin" || auth.Status != "aktif" {
		t.Fatalf("auth: %+v %v", auth, err)
	}

	var created string
	var change service.Change
	err = pgx.BeginFunc(e.ctx, e.svc.Pool(), func(tx pgx.Tx) error {
		u, ch, err := e.svc.CreateUser(e.ctx, tx, "+6281300001234", "Nadia Putri")
		if err != nil {
			return err
		}
		created, change = u.ID, ch
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Publish(change)
	u, _ = e.svc.FindUserByPhone(e.ctx, "081300001234")
	if u == nil || u.ID != created || u.Verification.Status != "belum" || change.Version == 0 {
		t.Fatalf("created %+v change %+v", u, change)
	}
	if n := e.state().Notifications[0]; n.UserID != created || n.Title != "Selamat datang di Sewain!" {
		t.Fatalf("welcome notification %+v", n)
	}
	// Duplicate phone is a rule violation and the caller's transaction can roll back cleanly.
	err = pgx.BeginFunc(e.ctx, e.svc.Pool(), func(tx pgx.Tx) error {
		_, _, err := e.svc.CreateUser(e.ctx, tx, "081300001234", "Lagi")
		return err
	})
	if msg, ok := service.IsUserError(err); !ok || msg != "Nomor HP sudah terdaftar. Masuk pakai nomor itu." {
		t.Fatalf("duplicate: %v", err)
	}

	// Demo sessions store only the hash.
	tok, err := e.svc.CreateDemoSession(e.ctx, "u-raka")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := e.svc.Pool().QueryRow(e.ctx, `SELECT token_hash FROM demo_sessions`).Scan(&stored); err != nil || stored == tok || stored != service.HashToken(tok) {
		t.Fatalf("token must be stored hashed: %v", err)
	}
	if uid, _ := e.svc.DemoSessionUser(e.ctx, tok); uid != "u-raka" {
		t.Fatalf("resolve token: %s", uid)
	}
}
