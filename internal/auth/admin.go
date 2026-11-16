package auth

import (
	"context"
	"fmt"
)

// GrantAdmin sets role=admin for the user with this phone (any format NormalizePhone accepts) and returns
// the user ID. It is meant for the `api admin grant <phone>` CLI. It writes users.role directly; the core
// users table owns that column (ROADMAP §3).
func GrantAdmin(ctx context.Context, db DB, phone string) (string, error) {
	return setRole(ctx, db, phone, RoleAdmin)
}

// RevokeAdmin sets role=user for the user with this phone (`api admin revoke <phone>`).
func RevokeAdmin(ctx context.Context, db DB, phone string) (string, error) {
	return setRole(ctx, db, phone, RoleUser)
}

func setRole(ctx context.Context, db DB, phone, role string) (string, error) {
	p, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	var id string
	err = db.QueryRow(ctx, `UPDATE users SET role = $2 WHERE phone = $1 RETURNING id`, p, role).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("auth: no user with phone %s (they must sign in once first): %w", p, err)
	}
	return id, nil
}
