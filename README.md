# Enlite

Enlite connects Ent to the cgo-free SQLite driver from liustack/sqlite. Use it with Ent's generated client, schema migrations, queries, and transactions. [Ent](https://entgo.io/) · [liustack/sqlite](https://github.com/liustack/sqlite)

## Install

```sh
go get github.com/lib-x/enlite@v0.1.0
```

Requires Go 1.26.6 through Go 1.27.x. The upstream driver supports Linux and macOS on amd64 and arm64, and Windows on amd64. Builds work with `CGO_ENABLED=0`.

## Quickstart

Pass the Enlite driver to `ent.NewClient` after generating your client. [Ent getting-started guide](https://entgo.io/docs/getting-started)

```go
import (
    "context"

    "example.com/your-app/ent"
    "github.com/lib-x/enlite"
)

func openClient(ctx context.Context) (*ent.Client, error) {
    drv, err := enlite.Open("app.db")
    if err != nil {
        return nil, err
    }
    client := ent.NewClient(ent.Driver(drv))
    if err := client.Schema.Create(ctx); err != nil {
        client.Close()
        return nil, err
    }
    return client, nil // Call client.Close() when finished.
}
```

`Open` validates the DSN and options. Use `drv.DB().PingContext(ctx)` to check the connection before the first query. Foreign keys are enabled by default on every new connection. Run `client.Schema.Create(ctx)` before serving queries and serialize migrations that use the same generated Ent schema in one process.

Run the quickstart from this repository to create a user with Ent and query SQLite using the upstream `regexp` extension. [Quickstart source](examples/quickstart/main.go)

```sh
CGO_ENABLED=0 go run ./examples/quickstart
# Ada matches ^A: true
```

## File database options

```go
drv, err := enlite.Open("file:app.db?mode=rwc",
    enlite.WithPragma("journal_mode(WAL)"),
    enlite.WithPragma("synchronous(NORMAL)"),
    enlite.WithBusyTimeout(5*time.Second),
    enlite.WithTxLock(enlite.TxImmediate),
    enlite.WithWALLimit(32<<20),
    enlite.WithPoolSize(4),
    enlite.WithSQLiteOptions(regexp.Option()),
)
```

| Option | Behavior |
| --- | --- |
| `WithPragma(expr)` | Runs a trusted SQL PRAGMA expression on each connection. PRAGMAs run in order, starting with `foreign_keys(1)`, then the DSN's `_pragma` entries, then the supplied options. |
| `WithBusyTimeout(duration)` | Sets the lock wait limit in whole milliseconds. The upstream default is five seconds. Zero disables waiting on locks. |
| `WithTxLock(mode)` | Sets `_txlock` to `TxDeferred`, `TxImmediate`, or `TxExclusive`. With `TxImmediate`, a read-then-write transaction acquires the writer lock at `BEGIN`. Read-only transactions begin deferred. |
| `WithWALLimit(bytes)` | Sets the size at which the upstream driver attempts to restart the WAL. The default is 64 MiB. Zero disables the size-based restart. |
| `WithPoolSize(n)` | Caps open and idle connections at the same positive value. Idle connections keep their page caches. File databases use `database/sql` defaults when this option is omitted. |
| `WithSQLiteOptions(opts...)` | Adds upstream `sqlite.Option` values to this connector, including functions, aggregates, collations, virtual tables, and extensions. Import `github.com/liustack/sqlite/ext/regexp` for `regexp.Option()`. |

WAL lets readers proceed while a writer holds the write lock. SQLite still serializes writes. With `synchronous(NORMAL)` in WAL mode, a power loss can roll back a recently committed transaction. [Upstream group commit](https://github.com/liustack/sqlite/blob/v0.1.7/docs/group-commit.md) · [Upstream backup and restore](https://github.com/liustack/sqlite/blob/v0.1.7/docs/replication.md)

## In-memory databases

```go
private, err := enlite.Open(":memory:")
shared, err := enlite.Open("file:/my-app?vfs=memdb", enlite.WithPoolSize(4))
```

`Open` uses one open and idle connection for a private in-memory database. A slash-prefixed `vfs=memdb` name lets connections share one in-memory database in the same process. Its contents disappear after the last connection closes. The upstream driver rejects `cache=shared`.

## Use `ent.Open("sqlite3", dsn)`

For the same registration style, import the optional `sqlite3` package and enable foreign keys in the DSN. [entsqlite](https://github.com/lib-x/entsqlite)

```go
import _ "github.com/lib-x/enlite/sqlite3"

// Inside a function with your generated ent package in scope.
client, err := ent.Open("sqlite3", "file:app.db?_pragma=foreign_keys(1)")
```

Only one driver can register `sqlite3`. Use `enlite.Open` when you need per-pool options or when the process already registers that name. The upstream `liustack/sqlite` package also registers `sqlite`, so it conflicts with `modernc.org/sqlite` and `github.com/glebarez/go-sqlite` in the same binary.

## Development

```sh
go generate ./examples/quickstart/ent
CGO_ENABLED=0 go test ./...
go test -race ./...
```

The Ent schema and generated client are checked in so the quickstart also runs from a fresh checkout.
