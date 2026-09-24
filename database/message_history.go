package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"iChat/models"

	"gorm.io/gorm"
)

// loadPersistedHistory never treats a cache hit as evidence of completeness.
// The legacy schema has no committed sequence: use a deterministic tuple for
// backward pagination, with the stored cursor (not client-supplied sort fields).
func (mm *msgManager) loadPersistedHistory(ctx context.Context, uid, target, kind uint, cursor models.Message, limit int) ([]string, error) {
	var messages []models.Message
	err := mm.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Authorization, cursor resolution and page selection share one snapshot.
		// Every page rechecks membership; deleted relations/groups grant no access.
		if kind == models.GroupType {
			var membership int64
			err := tx.Model(&models.Relation{}).
				Joins("JOIN `group` AS history_group ON history_group.id = relation.target_id AND history_group.deleted_at IS NULL").
				Where("relation.owner_id = ? AND relation.target_id = ? AND relation.type = ?", uid, target, models.GroupRelation).
				Count(&membership).Error
			if err != nil {
				return err
			}
			if membership == 0 {
				return ErrMessageHistoryForbidden
			}
		}
		conversation := func() *gorm.DB {
			query := tx.Model(&models.Message{}).Where("type = ?", kind)
			if kind == models.GroupType {
				return query.Where("receiver_id = ?", target)
			}
			return query.Where("((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))", uid, target, target, uid)
		}
		query := conversation()
		if cursor.Type != models.InvalidType || cursor.Id != "" {
			var stored models.Message
			if err := conversation().Where("id = ?", cursor.Id).Take(&stored).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrMessageCursorNotFound
				}
				return err
			}
			query = query.Where("time_stamp < ? OR (time_stamp = ? AND identifier < ?) OR (time_stamp = ? AND identifier = ? AND id < ?)",
				stored.TimeStamp, stored.TimeStamp, stored.Identifier, stored.TimeStamp, stored.Identifier, stored.Id)
		}
		return query.Order("time_stamp DESC").Order("identifier DESC").Order("id DESC").Limit(limit).Find(&messages).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	rows := make([]string, len(messages))
	for i := range messages {
		raw, err := json.Marshal(&messages[i])
		if err != nil {
			return nil, err
		}
		rows[i] = string(raw)
	}
	return rows, nil
}
