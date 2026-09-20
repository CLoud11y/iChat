package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/models"
	"unicode/utf8"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProcessDirtyMsgs returns only the length of the durably handled consecutive
// prefix. Each row has its own transaction; a failed/uncertain COMMIT is never
// included, even if MySQL may have committed it. Replays are idempotent.
func (mm *msgManager) ProcessDirtyMsgs(ctx context.Context, rawMsgs []string) (int, error) {
	if mm == nil || mm.db == nil || mm.db.Config == nil {
		return 0, ErrStorageUnavailable
	}
	if len(rawMsgs) > DirtyMessageBatchSize {
		return 0, fmt.Errorf("dirty message batch exceeds %d", DirtyMessageBatchSize)
	}
	for i, raw := range rawMsgs {
		if err := ctx.Err(); err != nil {
			return i, storageError("process dirty message", err)
		}
		if err := mm.processDirtyMessage(ctx, raw); err != nil {
			return i, storageError("process dirty message", err)
		}
	}
	return len(rawMsgs), nil
}

func (mm *msgManager) processDirtyMessage(ctx context.Context, raw string) error {
	digest := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(digest[:])
	msg, reason, detail := decodeDirtyMessage(raw)
	return mm.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A previous isolation remains authoritative, including after schema or
		// validation changes; retrying a queue ACK must not silently replay it.
		var previous models.LegacyMessageQuarantine
		result := tx.Where("payload_hash = ?", hash).Find(&previous)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 0 {
			return verifyQuarantineBody(previous, raw)
		}
		if reason == "" {
			if err := persistLegacyMessage(tx, &msg); err != nil {
				reason = quarantineReason(err)
				if reason == "" {
					return err
				}
				detail = err.Error()
			} else {
				return nil
			}
		}
		record := models.LegacyMessageQuarantine{PayloadHash: hash, RawBody: []byte(raw), ReasonCode: reason, ReasonDetail: detail}
		if err := tx.Create(&record).Error; err != nil {
			var duplicate *mysqlDriver.MySQLError
			if !errors.Is(err, gorm.ErrDuplicatedKey) && !(errors.As(err, &duplicate) && duplicate.Number == 1062) {
				return err
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("payload_hash = ?", hash).Take(&previous).Error; err != nil {
				return err
			}
			return verifyQuarantineBody(previous, raw)
		}
		return nil
	})
}

func verifyQuarantineBody(record models.LegacyMessageQuarantine, raw string) error {
	if !bytes.Equal(record.RawBody, []byte(raw)) {
		return errors.New("quarantine hash collision or corrupted raw body")
	}
	return nil
}

func decodeDirtyMessage(raw string) (models.Message, string, string) {
	var msg models.Message
	if !utf8.ValidString(raw) {
		return msg, "INVALID_JSON", "raw message is not valid UTF-8"
	}
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return msg, "INVALID_JSON", err.Error()
	}
	if msg.Id == "" || !validMessage(&msg) || msg.Media > models.AudioMedia {
		return msg, "INVALID_MESSAGE", "missing ID/participants or unsupported message/media type"
	}
	return msg, "", ""
}

// Only known statement-level data failures may be isolated. Connection loss,
// missing schema, deadlocks, lock timeouts and all unknown errors retain the
// queue responsibility. COMMIT errors bypass this classifier entirely.
func quarantineReason(err error) string {
	if errors.Is(err, ErrMessageConflict) {
		return "ID_CONFLICT"
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1264, 1366, 1406, 3819:
			return fmt.Sprintf("SQL_DATA_%d", mysqlErr.Number)
		}
	}
	return ""
}
