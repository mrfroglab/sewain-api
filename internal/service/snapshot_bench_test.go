package service_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/db/dbtest"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// BenchmarkSnapshot measures GET /api/v1/state's two halves at pilot scale (PRD §15.2: up to 3,000 users):
// building the scoped snapshot (one REPEATABLE READ transaction) and encoding it. The load test
// (docs/loadtest/2026-11-report.md) found the snapshot to be the CPU hot spot; this keeps it measurable.
//
//	TEST_DATABASE_URL=$(scripts/test-db.sh up) scripts/go.sh test ./internal/service -run '^$' -bench Snapshot -benchmem
func BenchmarkSnapshot(b *testing.B) {
	pool := dbtest.NewPool(b)
	ctx := context.Background()
	svc := service.New(pool, service.Options{DemoMode: true, SnapshotScope: service.ScopeScoped, Now: time.Now})
	if _, err := svc.ResetDemo(ctx); err != nil {
		b.Fatalf("seed: %v", err)
	}
	const users = 3000
	for i := 0; i < users; i++ {
		in := domain.RegisterInput{Name: fmt.Sprintf("Penguji %d", i), Phone: fmt.Sprintf("0896%08d", i), AreaID: "ugm"}
		if _, err := svc.Dispatch(ctx, nil, "registerUser", args(in), fmt.Sprintf("u-bench%d", i)); err != nil {
			b.Fatalf("register %d: %v", i, err)
		}
	}
	viewers := []struct {
		name string
		v    *service.Viewer
	}{{"guest", nil}, {"user", &service.Viewer{UserID: "u-dina", Role: "user"}}}
	for _, mode := range []string{service.SnapshotUsersAll, service.SnapshotUsersReferenced} {
		svc := service.New(pool, service.Options{DemoMode: true, SnapshotScope: service.ScopeScoped, SnapshotUsers: mode, Now: time.Now})
		for _, vw := range viewers {
			v := vw.v
			snap, err := svc.Snapshot(ctx, v)
			if err != nil {
				b.Fatal(err)
			}
			body, _ := service.SnapshotJSON(snap)
			b.Run(mode+"/"+vw.name+"/build", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := svc.Snapshot(ctx, v); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(mode+"/"+vw.name+"/etag", func(b *testing.B) { // the 304 check, per-viewer version (load test 7.2)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, _, err := svc.ETag(ctx, v); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(mode+"/"+vw.name+"/json", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for i := 0; i < b.N; i++ {
					if _, err := service.SnapshotJSON(snap); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
