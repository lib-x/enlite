package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lib-x/enlite"
	"github.com/lib-x/enlite/examples/quickstart/ent"
	"github.com/lib-x/enlite/examples/quickstart/ent/user"
	_ "github.com/lib-x/enlite/sqlite3"
)

// Ent's migrator mutates the generated table descriptors, so tests that
// migrate this schema run serially. TestConcurrentEntWrites then exercises
// concurrent client calls after migration.
func TestQuickstart(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Ada matches ^A: true\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestEntClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) (*ent.Client, error)
	}{
		{"connector", func(t *testing.T) (*ent.Client, error) {
			drv, err := enlite.Open(":memory:")
			if err != nil {
				return nil, err
			}
			return ent.NewClient(ent.Driver(drv)), nil
		}},
		{"sqlite3 registration", func(t *testing.T) (*ent.Client, error) {
			dsn := filepath.Join(t.TempDir(), "users.db") + "?_pragma=foreign_keys(1)"
			return ent.Open("sqlite3", dsn)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			client, err := tc.open(t)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if err := client.Schema.Create(ctx); err != nil {
				t.Fatalf("create schema: %v", err)
			}
			manager, err := client.User.Create().SetName("Ada").Save(ctx)
			if err != nil {
				t.Fatal(err)
			}
			report, err := client.User.Create().SetName("Grace").SetManager(manager).Save(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Schema.Create(ctx); err != nil {
				t.Fatalf("rerun migration: %v", err)
			}
			found, err := report.QueryManager().Only(ctx)
			if err != nil || found.ID != manager.ID {
				t.Fatalf("query manager = %v, err = %v, want %d", found, err, manager.ID)
			}
			if _, err := client.User.Create().SetName("Ada").Save(ctx); !ent.IsConstraintError(err) {
				t.Fatalf("duplicate name error = %v, want Ent constraint error", err)
			}
			if _, err := client.User.Create().SetName("Orphan").SetManagerID(manager.ID + report.ID + 1).Save(ctx); !ent.IsConstraintError(err) {
				t.Fatalf("invalid manager error = %v, want Ent constraint error", err)
			}
			updated, err := report.Update().SetName("Grace Hopper").Save(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Name != "Grace Hopper" {
				t.Fatalf("updated name = %q, want Grace Hopper", updated.Name)
			}
			if err := client.User.DeleteOneID(report.ID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.User.Get(ctx, report.ID); !ent.IsNotFound(err) {
				t.Fatalf("query deleted user error = %v, want Ent not found error", err)
			}

			for _, commit := range []bool{true, false} {
				name := fmt.Sprintf("committed=%t", commit)
				tx, err := client.Tx(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.User.Create().SetName(name).Save(ctx); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if commit {
					err = tx.Commit()
				} else {
					err = tx.Rollback()
				}
				if err != nil {
					t.Fatal(err)
				}
				if exists, err := client.User.Query().Where(user.NameEQ(name)).Exist(ctx); err != nil || exists != commit {
					t.Fatalf("user %q exists = %v, err = %v, want %v", name, exists, err, commit)
				}
			}
		})
	}
}

func TestConcurrentEntWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	drv, err := enlite.Open(filepath.Join(t.TempDir(), "concurrent.db"),
		enlite.WithPragma("journal_mode(WAL)"), enlite.WithPoolSize(4))
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if _, err := client.User.Create().SetName(fmt.Sprintf("user-%d", i)).Save(ctx); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	if count, err := client.User.Query().Count(ctx); err != nil || count != writers {
		t.Fatalf("user count = %d, err = %v, want %d", count, err, writers)
	}
}
