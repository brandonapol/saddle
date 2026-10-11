package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// SchemaVersion is the state.db schema this binary writes: the number of
// migrations, stored in PRAGMA user_version. A release that changes it is a
// minor bump at least (docs/RELEASING.md).
func SchemaVersion() int { return len(migrations) }

// FileSchemaVersion reads the schema version of the database at path
// without migrating it.
func FileSchemaVersion(path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	err = db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// Backup writes a consistent copy of the database at src to dst with
// VACUUM INTO, which is safe while other processes use src. It refuses to
// overwrite dst.
func Backup(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup %s already exists", dst)
	}
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	return backupDB(db, dst)
}

func backupDB(db *sql.DB, dst string) error {
	_, err := db.Exec(`VACUUM INTO ?`, dst)
	return err
}

// backupBeforeMigrate copies an existing database that this binary is about
// to migrate to path.bak-schema<N>-<time>, so an upgrade can be rolled back.
// A new (version 0) or current database is left alone.
func backupBeforeMigrate(db *sql.DB, path string, now time.Time) (string, error) {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return "", err
	}
	if v == 0 || v >= len(migrations) {
		return "", nil
	}
	dst := fmt.Sprintf("%s.bak-schema%d-%s", path, v, now.UTC().Format("20060102T150405Z"))
	if err := backupDB(db, dst); err != nil {
		// Another process opening the same database in the same second
		// already wrote this backup.
		if _, serr := os.Stat(dst); serr == nil && strings.Contains(err.Error(), "exists") {
			return dst, nil
		}
		return "", errors.Join(fmt.Errorf("back up %s before migrating schema %d to %d", path, v, len(migrations)), err)
	}
	return dst, nil
}
