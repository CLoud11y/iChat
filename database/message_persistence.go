package database

import (
	"errors"
	"fmt"
	"iChat/models"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrMessageConflict = errors.New("legacy message ID has different persisted content")

// The legacy global message primary key remains the identity until P2.
// Never UPDATE/IGNORE a duplicate: all persisted fields must match exactly.
func persistLegacyMessage(tx *gorm.DB, msg *models.Message) error {
	err := tx.Create(msg).Error
	if err == nil {
		return nil
	}
	var duplicate *mysqlDriver.MySQLError
	if !errors.Is(err, gorm.ErrDuplicatedKey) && !(errors.As(err, &duplicate) && duplicate.Number == 1062) {
		return err
	}
	var existing models.Message
	// A locking read observes the committed row after unique-key contention,
	// rather than relying on a possibly older repeatable-read snapshot.
	if readErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", msg.Id).Take(&existing).Error; readErr != nil {
		return fmt.Errorf("verify duplicate message: %w", errors.Join(err, readErr))
	}
	if existing != *msg {
		return fmt.Errorf("%w: %s", ErrMessageConflict, msg.Id)
	}
	return nil
}
