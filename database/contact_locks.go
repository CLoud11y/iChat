package database

import (
	"errors"
	"iChat/models"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Contact writers lock parent users by numeric ID before any group/relation.
// The shared permission reads use the same order. Future revocation/restore
// methods must follow this order too; transactions may never reverse it.
func lockContactUsers(tx *gorm.DB, ids ...uint) ([]models.User, error) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var users []models.User
	for i, id := range ids {
		if i > 0 && id == ids[i-1] {
			continue
		}
		var user models.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "phone").Where("id = ?", id).Take(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrUserNotFound
			}
			return nil, err
		}
		users = append(users, user)
	}
	return users, nil
}

func lockContactGroup(tx *gorm.DB, groupID uint) (*models.Group, error) {
	var group models.Group
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", groupID).Take(&group).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrGroupNotFound
		}
		return nil, err
	}
	return &group, nil
}
