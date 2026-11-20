package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/lib-x/enlite"
	"github.com/lib-x/enlite/examples/quickstart/ent"
	"github.com/lib-x/enlite/examples/quickstart/ent/user"
	"github.com/liustack/sqlite/ext/regexp"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, out io.Writer) error {
	drv, err := enlite.Open(":memory:",
		enlite.WithBusyTimeout(time.Second),
		enlite.WithSQLiteOptions(regexp.Option()),
	)
	if err != nil {
		return fmt.Errorf("open SQLite: %w", err)
	}
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := client.User.Create().SetName("Ada").Save(ctx); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	user, err := client.User.Query().Where(user.NameEQ("Ada")).Only(ctx)
	if err != nil {
		return fmt.Errorf("query user: %w", err)
	}
	var matches bool
	err = drv.DB().QueryRowContext(ctx, "SELECT ? REGEXP ?", user.Name, "^A").Scan(&matches)
	if err != nil {
		return fmt.Errorf("match user name: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s matches ^A: %t\n", user.Name, matches)
	return err
}
