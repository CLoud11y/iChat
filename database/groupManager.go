package database

import (
	"context"
	"iChat/models"

	"gorm.io/gorm"
)

const MAX_GROUP_USER = 50

var Gmanager *groupManager

type groupManager struct {
	db *gorm.DB
}

func NewGroupManager(db *gorm.DB) *groupManager {
	return &groupManager{
		db: db,
	}
}

func (gm *groupManager) JoinGroup(ctx context.Context, userId uint, groupId uint) error {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return ErrStorageUnavailable
	}
	return NewRelationManager(gm.db).AddGroupRelation(ctx, userId, groupId)
}

func (gm *groupManager) CreateGroup(ctx context.Context, name string, ownerId uint, desc string) error {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return ErrStorageUnavailable
	}
	// TODO: 限制用户创建的群聊数量
	group := models.Group{
		Name:    name,
		OwnerId: ownerId,
		Desc:    desc,
		MaxUser: MAX_GROUP_USER,
	}
	err := gm.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&group).Error; err != nil {
			return err
		}
		// The group and its owner membership must commit together.
		return tx.Create(&models.Relation{OwnerId: ownerId, TargetId: group.ID, Type: models.GroupRelation}).Error
	})
	if err != nil {
		return storageError("create group", err)
	}
	return nil
}

func (gm *groupManager) DeleteGroup(ctx context.Context, ownerId uint, groupId uint) error {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return ErrStorageUnavailable
	}
	result := gm.db.WithContext(ctx).Delete(&models.Group{}, "owner_id =? AND id =?", ownerId, groupId)
	if result.Error != nil {
		return storageError("delete group", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrGroupNotOwned
	}
	return nil
}

// 查询用户加入的全部群聊信息
func (gm *groupManager) GetGroupsByUid(ctx context.Context, userId uint) ([]models.Group, error) {
	objIds, err := gm.GetGroupIds(ctx, userId)
	if err != nil {
		return nil, err
	}
	return gm.GetGroupsById(ctx, objIds)
}

// 多表联查提高性能
func (gm *groupManager) GetGroupsByUid2(ctx context.Context, userId uint) ([]models.Group, error) {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return nil, ErrStorageUnavailable
	}
	groups := []models.Group{}
	err := gm.db.WithContext(ctx).InnerJoins("JOIN relation ON group.id = relation.target_id AND relation.owner_id = ? AND relation.type = ?",
		userId, models.GroupRelation).Find(&groups).Error
	if err != nil {
		return nil, storageError("find groups by user", err)
	}
	return groups, nil
}

// 查询用户加入的群聊id
func (gm *groupManager) GetGroupIds(ctx context.Context, userId uint) ([]uint, error) {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return nil, ErrStorageUnavailable
	}
	relations := make([]models.Relation, 0)
	err := gm.db.WithContext(ctx).Where("owner_id = ? and type = ?", userId, models.GroupRelation).Find(&relations).Error
	if err != nil {
		return nil, storageError("find group relations", err)
	}
	objIds := make([]uint, 0)
	for _, relation := range relations {
		objIds = append(objIds, relation.TargetId)
	}
	return objIds, nil
}

// 通过·群聊id·查询群聊信息
func (gm *groupManager) GetGroupsById(ctx context.Context, groupIds []uint) ([]models.Group, error) {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return nil, ErrStorageUnavailable
	}
	groups := []models.Group{}
	err := gm.db.WithContext(ctx).Find(&groups, "id IN (?)", groupIds).Error
	if err != nil {
		return nil, storageError("find groups by ID", err)
	}
	return groups, nil
}

func (gm *groupManager) GetGroupUsers(ctx context.Context, groupId uint) ([]models.User, error) {
	if gm == nil || gm.db == nil || gm.db.Config == nil {
		return nil, ErrStorageUnavailable
	}
	users := []models.User{}
	err := gm.db.WithContext(ctx).InnerJoins("JOIN relation ON user.id = relation.owner_id AND relation.target_id = ? AND relation.type = ?",
		groupId, models.GroupRelation).Find(&users).Error
	if err != nil {
		return nil, storageError("find group users", err)
	}
	return users, nil
}
