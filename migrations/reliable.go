// Package migrations owns explicit, versioned database schema changes.
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed 001_reliable.sql
var schemaFS embed.FS

const version = 1
const marker = "reliability_schema_migration"

var reliableTables = []string{"conversation", "message", "message_command", "message_outbox", "message_delivery_cursor", "message_quarantine"}

// ApplyReliable only creates a fresh reliability schema. A partially applied,
// legacy, changed or unknown-version schema needs an explicit operator repair;
// it is never silently upgraded or reset.
func ApplyReliable(ctx context.Context, db *sql.DB) (err error) {
	if db == nil {
		return errors.New("reliable migration: mysql is not initialized")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reliable migration connection: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	var schema string
	if err = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
		return fmt.Errorf("reliable migration requires a selected database: %w", err)
	}
	if schema == "" {
		return errors.New("reliable migration requires a selected database")
	}
	lockName := fmt.Sprintf("ichat:reliable:%x", sha256.Sum256([]byte(schema)))[:63]
	var locked sql.NullInt64
	if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 10)", lockName).Scan(&locked); err != nil {
		return fmt.Errorf("acquire reliable migration lock: %w", err)
	}
	if !locked.Valid || locked.Int64 != 1 {
		return errors.New("reliable migration lock unavailable")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released sql.NullInt64
		releaseErr := conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", lockName).Scan(&released)
		if releaseErr == nil && (!released.Valid || released.Int64 != 1) {
			releaseErr = errors.New("migration lock was not held on release")
		}
		if releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release reliable migration lock: %w", releaseErr))
		}
	}()
	return applyLocked(ctx, conn)
}

func applyLocked(ctx context.Context, conn *sql.Conn) error {
	body, err := schemaFS.ReadFile("001_reliable.sql")
	if err != nil {
		return err
	}
	// Git checkouts may use CRLF or LF; migration identity is platform-neutral.
	body = []byte(strings.ReplaceAll(string(body), "\r\n", "\n"))
	definitionHash := sha256.Sum256(body)
	tables, err := existingTables(ctx, conn)
	if err != nil {
		return err
	}
	_, hasMarker := tables[marker]
	if hasMarker {
		var storedDefinition, storedSchema []byte
		var storedVersion uint16
		err := conn.QueryRowContext(ctx, "SELECT version, definition_hash, schema_hash FROM `reliability_schema_migration`").Scan(&storedVersion, &storedDefinition, &storedSchema)
		if err == nil {
			if storedVersion != version || string(storedDefinition) != string(definitionHash[:]) {
				return errors.New("reliable migration version or definition differs from applied version")
			}
			fingerprint, err := schemaFingerprint(ctx, conn)
			if err != nil {
				return err
			}
			if string(storedSchema) != string(fingerprint[:]) {
				return errors.New("reliable schema drift detected")
			}
			var count int
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `reliability_schema_migration`").Scan(&count); err != nil || count != 1 {
				return fmt.Errorf("invalid reliable migration version count %d: %w", count, err)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read reliable migration version: %w", err)
		}
	}
	if len(tables) > 0 && !(len(tables) == 1 && hasMarker) {
		return errors.New("refusing reliable migration on legacy or partial schema; use an explicit reset or repair")
	}
	if !hasMarker {
		_, err = conn.ExecContext(ctx, "CREATE TABLE `reliability_schema_migration` (`version` smallint unsigned NOT NULL PRIMARY KEY, `definition_hash` binary(32) NOT NULL, `schema_hash` binary(32) NOT NULL, `applied_at` datetime(6) NOT NULL) ENGINE=InnoDB")
		if err != nil {
			return fmt.Errorf("create reliable migration marker: %w", err)
		}
	}
	statements := strings.Split(string(body), ";")
	if len(statements) != len(reliableTables)+1 || strings.TrimSpace(statements[len(statements)-1]) != "" {
		return errors.New("reliable schema definition has unexpected statement count")
	}
	for i, stmt := range statements[:len(reliableTables)] {
		if !strings.HasPrefix(strings.TrimSpace(stmt), "CREATE TABLE `"+reliableTables[i]+"`") {
			return fmt.Errorf("unexpected reliable schema statement %d", i+1)
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create %s: %w", reliableTables[i], err)
		}
	}
	fingerprint, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO `reliability_schema_migration` (`version`,`definition_hash`,`schema_hash`,`applied_at`) VALUES (?,?,?,UTC_TIMESTAMP(6))", version, definitionHash[:], fingerprint[:]); err != nil {
		return fmt.Errorf("record reliable schema version: %w", err)
	}
	return nil
}

func existingTables(ctx context.Context, conn *sql.Conn) (map[string]struct{}, error) {
	rows, err := conn.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		return nil, fmt.Errorf("list schema tables: %w", err)
	}
	defer rows.Close()
	tables := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = struct{}{}
	}
	return tables, rows.Err()
}

func schemaFingerprint(ctx context.Context, conn *sql.Conn) ([32]byte, error) {
	hash := sha256.New()
	for _, name := range reliableTables {
		var tableName, ddl string
		if err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE `"+name+"`").Scan(&tableName, &ddl); err != nil {
			return [32]byte{}, fmt.Errorf("read %s definition: %w", name, err)
		}
		fmt.Fprintf(hash, "%d:%s%d:%s", len(tableName), tableName, len(ddl), ddl)
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}
