package enlite

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/liustack/sqlite/replica"
)

// BackupDriver is an Ent SQLite driver with an active S3-compatible backup.
// Close it through the Ent client or directly when you finish using it.
type BackupDriver struct {
	*entsql.Driver
	backup *replica.Replica
	close  func() error
}

// Close closes the SQLite pool first, then stops and flushes the backup.
// Repeated calls return the result of the first close.
func (d *BackupDriver) Close() error { return d.close() }

// SyncBackup waits until the changes committed before the call are uploaded.
func (d *BackupDriver) SyncBackup(ctx context.Context) error { return d.backup.Sync(ctx) }

// BackupStatus reports staging and upload progress, including upload errors.
func (d *BackupDriver) BackupStatus() replica.Status { return d.backup.Status() }

// WithBackup configures the replica used by [OpenWithBackup]. Set URL when the
// DSN is a local path or file: URI. With an S3 DSN, omit URL and use cfg to set
// options such as credentials, sync interval, retention or error reporting.
func WithBackup(cfg replica.Config) Option {
	return func(c *config) error {
		c.backup = &cfg
		return nil
	}
}

// WithRestoreOnMissing restores the latest backup before opening if the local
// database file is absent. An empty backup prefix allows a fresh database.
// An existing database file is opened as is.
func WithRestoreOnMissing() Option {
	return func(cfg *config) error {
		cfg.restoreOnMissing = true
		return nil
	}
}

// OpenWithBackup starts a replica for a writable local SQLite file and returns
// an Ent driver. A DSN such as s3://bucket/prefix?local=app.db&region=us-east-1
// sets both the local path and the backup address. A local path or file: DSN can
// also be used with [WithBackup]. The context covers startup and any restore.
// The replica manages WAL and checkpoints. Use one writer per remote backup
// prefix, and close the driver or its Ent client to flush and stop the replica.
func OpenWithBackup(ctx context.Context, dsn string, opts ...Option) (*BackupDriver, error) {
	localDSN, remoteURL, err := splitBackupDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(localDSN, opts)
	if err != nil {
		return nil, err
	}
	var backupConfig replica.Config
	if cfg.backup != nil {
		backupConfig = *cfg.backup
	}
	if remoteURL != "" {
		if backupConfig.URL != "" {
			return nil, errors.New("enlite: provide the backup URL in the S3 DSN or WithBackup")
		}
		backupConfig.URL = remoteURL
	}
	if backupConfig.URL == "" {
		return nil, errors.New("enlite: backup URL is required")
	}
	path, err := backupPath(cfg)
	if err != nil {
		return nil, err
	}
	// Give SQLite and the replica the same absolute path. Escape URI delimiters
	// for SQLite while passing the native filesystem path to the replica.
	cfg.name = "file:" + (&url.URL{Path: path}).EscapedPath()
	connector, err := cfg.connector()
	if err != nil {
		return nil, err
	}
	if err := prepareBackupFile(ctx, path, cfg, backupConfig); err != nil {
		return nil, err
	}
	backupName := path
	if lastValue(cfg.params["mode"]) == "rw" {
		// The replica uses SQLite to open the file. Preserve rw even if the
		// file disappears while the replica checks the remote store.
		backupName = cfg.name + "?mode=rw"
	}
	backup, err := replica.New(ctx, backupName, backupConfig)
	if err != nil {
		return nil, fmt.Errorf("enlite: start backup: %w", err)
	}
	drv := cfg.driver(connector)
	return &BackupDriver{
		Driver: drv,
		backup: backup,
		close: sync.OnceValue(func() error {
			return errors.Join(drv.Close(), backup.Close())
		}),
	}, nil
}

func isS3DSN(dsn string) bool {
	scheme, _, ok := strings.Cut(dsn, "://")
	return ok && strings.EqualFold(scheme, "s3")
}

func splitBackupDSN(dsn string) (localDSN, remoteURL string, err error) {
	if !isS3DSN(dsn) {
		return dsn, "", nil
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return "", "", errors.New("enlite: S3 DSN must be s3://bucket/prefix?local=path with credentials supplied separately")
	}
	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", "", errors.New("enlite: invalid S3 DSN query")
	}
	local := params["local"]
	if len(local) != 1 || local[0] == "" || isS3DSN(local[0]) {
		return "", "", errors.New("enlite: S3 DSN requires one local SQLite DSN in the local parameter")
	}
	params.Del("local")
	for key := range params {
		if key != "endpoint" && key != "region" {
			return "", "", errors.New("enlite: S3 DSN accepts local, endpoint, and region query parameters")
		}
	}
	u.RawQuery = params.Encode()
	return local[0], u.String(), nil
}

func backupPath(cfg config) (string, error) {
	modes := cfg.params["mode"]
	if cfg.path == "" || cfg.path == ":memory:" || cfg.params.Has("vfs") ||
		cfg.params.Has("immutable") || cfg.params.Has("nolock") ||
		(len(modes) > 0 && (len(modes) != 1 || (modes[0] != "rw" && modes[0] != "rwc"))) {
		return "", errors.New("enlite: backup requires a writable local SQLite file with the default VFS and locking")
	}
	if tail, ok := strings.CutPrefix(cfg.name, "file://"); ok {
		authority, _, _ := strings.Cut(tail, "/")
		if authority != "" && authority != "localhost" {
			return "", errors.New("enlite: backup requires a local file URI")
		}
	}
	path := cfg.path
	if filepath.Separator == '\\' && strings.HasPrefix(cfg.name, "file:") {
		// SQLite URI paths can spell a Windows drive as /C:/path.
		if drivePath, ok := strings.CutPrefix(path, "/"); ok && filepath.VolumeName(drivePath) != "" {
			path = drivePath
		}
	}
	path, err := resolveBackupPath(path)
	if err != nil {
		return "", fmt.Errorf("enlite: resolve backup database path: %w", err)
	}
	return path, nil
}

func resolveBackupPath(path string) (string, error) {
	// Resolve symlinks before making the path absolute so that .. after a
	// symlink follows filesystem semantics instead of lexical cleanup.
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, lookupErr := os.Lstat(path); lookupErr == nil {
			// A dangling final symlink is never a missing restore target.
			return "", err
		} else if !errors.Is(lookupErr, os.ErrNotExist) {
			return "", lookupErr
		}
		dir, base := filepath.Split(path)
		if base == "" {
			return "", err
		}
		dir, err = filepath.EvalSymlinks(cmp.Or(dir, "."))
		resolved = filepath.Join(dir, base)
	}
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func prepareBackupFile(ctx context.Context, path string, cfg config, backupConfig replica.Config) error {
	mustExist := lastValue(cfg.params["mode"]) == "rw"
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("enlite: backup database must be a regular file")
		}
		return nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("enlite: check backup database: %w", statErr)
	}
	if !cfg.restoreOnMissing {
		if mustExist {
			return fmt.Errorf("enlite: check backup database: %w", statErr)
		}
		return nil
	}
	_, err := replica.Restore(ctx, path, backupConfig, replica.RestoreOptions{})
	if err != nil && (mustExist || !errors.Is(err, replica.ErrNoReplica)) {
		return fmt.Errorf("enlite: restore missing database: %w", err)
	}
	return nil
}
