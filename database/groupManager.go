package database

import (
	"fmt"
	"iChat/models"
	"iChat/utils"

	"gorm.io/gorm"
)

const MAX_GROUP_USER = 50

var Gmanager *groupManager

type groupManager struct {
	db *gorm.DB
}

func init() {
	Gmanager = &groupManager{
		db: utils.DB,
	}
}

func (gm *groupManager) JoinGroup(userId uint, groupId uint) error {
	//
	err := Rmanager.AddGroupRelation(userId, groupId)
	if err != nil {
		return err
	}
	return nil
}

func (gm *groupManager) CreateGroup(name string, ownerId uint, desc string) error {
	// TODO: 限制用户创建的群聊数量
	group := models.Group{
		Name:    name,
		OwnerId: ownerId,
		Desc:    desc,
		MaxUser: MAX_GROUP_USER,
	}
	fmt.Println(group)
	var err error
	tx := gm.db.Begin()
	defer func() {
		if res := recover(); res != nil {
			tx.Rollback()
		}
	}()
	if err = tx.Create(&group).Error; err != nil {
		tx.Rollback()
		return err
	}
	// 将创建者加入群聊
	err = Rmanager.AddGroupRelation(ownerId, group.ID)
	// 若失败 撤销创建的群聊
	if err != nil {
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

func (gm *groupManager) DeleteGroup(ownerId uint, groupId uint) error {
	gm.db.Delete(&models.Group{}, "owner_id =? AND id =?", ownerId, groupId)
	return nil
}

// 查询用户加入的全部群聊信息
func (gm *groupManager) GetGroupsByUid(userId uint) ([]models.Group, error) {
	objIds, err := gm.GetGroupIds(userId)
	if err != nil {
		return nil, err
	}
	return gm.GetGroupsById(objIds)
}

// 查询用户加入的群聊id
func (gm *groupManager) GetGroupIds(userId uint) ([]uint, error) {
	relations := make([]models.Relation, 0)
	err := Rmanager.db.Where("owner_id = ? and type = ?", userId, models.GroupRelation).Find(&relations).Error
	if err != nil {
		return nil, err
	}
	objIds := make([]uint, 0)
	for _, relation := range relations {
		objIds = append(objIds, relation.TargetId)
	}
	return objIds, nil
}

// 通过·群聊id·查询群聊信息
func (gm *groupManager) GetGroupsById(groupIds []uint) ([]models.Group, error) {
	groups := []models.Group{}
	err := gm.db.Find(&groups, "id IN (?)", groupIds).Error
	if err != nil {
		return nil, err
	}
	return groups, nil
}
