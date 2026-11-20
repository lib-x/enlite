package enlite_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib-x/enlite"
	"github.com/lib-x/enlite/examples/quickstart/ent"
	"github.com/liustack/sqlite/replica"
)

func backupTestConfig(endpoint string) replica.Config {
	return replica.Config{
		URL:       "s3://bucket/demo?" + url.Values{"endpoint": {endpoint}}.Encode(),
		AllowHTTP: true,
		// The tests each own their isolated bucket.
		SingleWriter: true,
		Credentials: replica.StaticCredentials(replica.Credentials{
			AccessKeyID: "test-key", SecretAccessKey: "test-secret",
		}),
	}
}

func TestBackupAndRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store := newBackupStore(t)
	config := backupTestConfig(store.URL)
	config.URL = ""
	root := t.TempDir()
	const filename = "source & #%.db"
	source := filepath.Join(root, filename)
	local := source
	if runtime.GOOS != "windows" {
		// Resolving .. after a symlink must select the target's parent.
		storage := filepath.Join(root, "storage")
		nested := filepath.Join(storage, "nested")
		if err := os.MkdirAll(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		shortcut := filepath.Join(root, "shortcut")
		if err := os.Symlink(nested, shortcut); err != nil {
			t.Fatal(err)
		}
		local = shortcut + "/../" + filename
		source = filepath.Join(storage, filename)
	}
	uri := "file:" + (&url.URL{Path: local}).EscapedPath()
	params := url.Values{"local": {uri}, "endpoint": {store.URL}}
	dsn := "s3://bucket/demo?" + params.Encode()
	drv, err := enlite.OpenWithBackup(ctx, dsn, enlite.WithBackup(config), enlite.WithRestoreOnMissing())
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.User.Create().SetName("Ada").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := drv.SyncBackup(ctx); err != nil {
		t.Fatal(err)
	}
	if status := drv.BackupStatus(); status.Uploaded == 0 || status.Uploaded != status.Staged {
		t.Fatalf("backup after sync: %+v", status)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := drv.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("database at requested path: %v", err)
	}

	params.Del("local")
	config.URL = "s3://bucket/demo?" + params.Encode()
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	restoredDSN := "file:" + (&url.URL{Path: restoredPath}).EscapedPath() + "?mode=rw"
	restored, err := enlite.OpenWithBackup(ctx, restoredDSN, enlite.WithBackup(config), enlite.WithRestoreOnMissing())
	if err != nil {
		t.Fatal(err)
	}
	restoredClient := ent.NewClient(ent.Driver(restored))
	t.Cleanup(func() { _ = restoredClient.Close() })
	got, err := restoredClient.User.Query().Only(ctx)
	if err != nil || got.Name != "Ada" {
		t.Fatalf("restored user = %v, err = %v, want Ada", got, err)
	}
	if err := restoredClient.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupS3Failure(t *testing.T) {
	t.Parallel()
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" && r.URL.Query().Get("prefix") == "demo/" {
			_, _ = io.WriteString(w, "<ListBucketResult/>")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
	}))
	t.Cleanup(store.Close)
	config := backupTestConfig(store.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	drv, err := enlite.OpenWithBackup(ctx, filepath.Join(t.TempDir(), "app.db"), enlite.WithBackup(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	if _, err := drv.DB().ExecContext(ctx, "CREATE TABLE data (id INTEGER)"); err != nil {
		t.Fatalf("local write: %v", err)
	}
	if err := drv.SyncBackup(ctx); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("sync against denied bucket: %v", err)
	}
	if closeErr := drv.Close(); closeErr == nil || drv.Close() != closeErr {
		t.Fatalf("Close should report and retain the upload error: %v", closeErr)
	}

	dir := t.TempDir()
	config.URL = strings.Replace(config.URL, "/demo?", "/forbidden?", 1)
	_, err = enlite.OpenWithBackup(ctx, filepath.Join(dir, "restored.db"), enlite.WithBackup(config), enlite.WithRestoreOnMissing())
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("restore against denied bucket: %v", err)
	}
	if paths, err := os.ReadDir(dir); err != nil || len(paths) > 0 {
		t.Fatalf("failed restore left files: %v, error: %v", paths, err)
	}
}

func TestBackupConfigurationErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		dsn  string
		opts []enlite.Option
		want string
	}{
		{name: "missing URL", dsn: "app.db", want: "backup URL is required"},
		{name: "missing local path", dsn: "s3://bucket/db", want: "one local SQLite DSN"},
		{name: "duplicate local path", dsn: "s3://bucket/db?local=a&local=b", want: "one local SQLite DSN"},
		{name: "nested S3", dsn: "s3://bucket/db?local=s3://bucket/b", want: "one local SQLite DSN"},
		{name: "missing prefix", dsn: "s3://bucket?local=app.db", want: "s3://bucket/prefix"},
		{name: "credentials in URL", dsn: "s3://user:secret@bucket/db?local=app.db", want: "credentials supplied separately"},
		{name: "unknown parameter", dsn: "s3://bucket/db?local=app.db&foo=value", want: "accepts local, endpoint, and region"},
		{name: "conflicting URL", dsn: "s3://bucket/db?local=app.db", opts: []enlite.Option{enlite.WithBackup(replica.Config{URL: "s3://other/db"})}, want: "provide the backup URL"},
		{name: "memory", dsn: "s3://bucket/db?local=:memory:", want: "writable local SQLite file"},
		{name: "shared memory", dsn: "s3://bucket/db?local=file:/shared%3Fvfs%3Dmemdb", want: "writable local SQLite file"},
		{name: "readonly", dsn: "s3://bucket/db?local=file:app.db%3Fmode%3Dro", want: "writable local SQLite file"},
		{name: "nonlocal authority", dsn: "s3://bucket/db?local=file://server/share/db", want: "local file URI"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			drv, err := enlite.OpenWithBackup(t.Context(), tt.dsn, tt.opts...)
			if err == nil || !strings.Contains(err.Error(), tt.want) || drv != nil {
				t.Fatalf("OpenWithBackup returned %v, %v, want error containing %q", drv, err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("OpenWithBackup exposed credential: %v", err)
			}
		})
	}
	missingPath := "file:" + (&url.URL{Path: filepath.Join(t.TempDir(), "missing.db")}).EscapedPath()
	_, err := enlite.OpenWithBackup(t.Context(), missingPath+"?mode=rw", enlite.WithBackup(replica.Config{URL: "s3://bucket/db"}))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file in mode=rw: %v", err)
	}
	config := enlite.WithBackup(replica.Config{URL: "s3://bucket/db"})
	if _, err := enlite.OpenWithBackup(t.Context(), t.TempDir(), config); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory as backup file: %v", err)
	}
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		link := filepath.Join(dir, "link.db")
		if err := os.Symlink(filepath.Join(dir, "missing.db"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := enlite.OpenWithBackup(t.Context(), link, config, enlite.WithRestoreOnMissing()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dangling symlink as restore target: %v", err)
		}
	}
	if _, err := enlite.Open("s3://bucket/db?local=app.db"); err == nil || !strings.Contains(err.Error(), "OpenWithBackup") {
		t.Fatalf("Open with an S3 DSN: %v", err)
	}
	if _, err := enlite.Open(":memory:", enlite.WithBackup(replica.Config{})); err == nil || !strings.Contains(err.Error(), "OpenWithBackup") {
		t.Fatalf("Open with backup options: %v", err)
	}
	drv, err := enlite.Open("s3:notes.db")
	if err != nil {
		t.Fatalf("Open with a plain filename containing a colon: %v", err)
	}
	_ = drv.Close()
}

// newBackupStore implements the S3 calls used for small backups and restores.
func newBackupStore(t *testing.T) *httptest.Server {
	t.Helper()
	type objectInfo struct {
		Key          string
		Size         int
		LastModified time.Time
	}
	type listResult struct {
		XMLName  xml.Name     `xml:"ListBucketResult"`
		Contents []objectInfo `xml:"Contents"`
	}
	var mu sync.Mutex
	objects := make(map[string][]byte)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if prefix := r.URL.Query().Get("prefix"); r.URL.Query().Get("list-type") == "2" {
			result := listResult{}
			for _, name := range slices.Sorted(maps.Keys(objects)) {
				if strings.HasPrefix(name, prefix) && name > r.URL.Query().Get("start-after") {
					result.Contents = append(result.Contents, objectInfo{name, len(objects[name]), time.Now().UTC()})
				}
			}
			_ = xml.NewEncoder(w).Encode(result)
			return
		}
		switch r.Method {
		case http.MethodPut:
			if _, exists := objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			objects[key] = data
		case http.MethodGet, http.MethodHead:
			data, exists := objects[key]
			if !exists {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, key, time.Time{}, bytes.NewReader(data))
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
