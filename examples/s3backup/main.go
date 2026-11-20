package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lib-x/enlite"
	"github.com/lib-x/enlite/examples/quickstart/ent"
	"github.com/lib-x/enlite/examples/quickstart/ent/user"
	"github.com/liustack/sqlite/replica"
)

func main() {
	dsn := flag.String("dsn", "", "backup DSN (s3://bucket/prefix?local=backup-demo.db)")
	flag.Parse()
	if *dsn == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := run(ctx, *dsn); err != nil {
		log.Fatal(err)
	}
	fmt.Println("S3 backup is up to date.")
}

func run(ctx context.Context, dsn string) (err error) {
	drv, err := enlite.OpenWithBackup(ctx, dsn,
		enlite.WithBackup(replica.Config{
			OnError: func(err error) { log.Printf("backup: %v", err) },
		}),
		enlite.WithRestoreOnMissing(),
	)
	if err != nil {
		return err
	}
	client := ent.NewClient(ent.Driver(drv))
	// Closing the client also closes and flushes the replica.
	defer func() { err = errors.Join(err, client.Close()) }()
	if err := client.Schema.Create(ctx); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	const name = "Ada"
	exists, err := client.User.Query().Where(user.NameEQ(name)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("look up demo user: %w", err)
	}
	if !exists {
		if _, err := client.User.Create().SetName(name).Save(ctx); err != nil {
			return fmt.Errorf("create demo user: %w", err)
		}
	}
	if err := drv.SyncBackup(ctx); err != nil {
		return fmt.Errorf("upload committed changes: %w", err)
	}
	return nil
}
