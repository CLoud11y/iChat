package database

import (
	"errors"
	"iChat/models"
	"iChat/utils"

	"gorm.io/gorm"
)

var Rmanager *relationManager

type relationManager struct {
	db *gorm.DB
}

func init() {
	Rmanager = &relationManager{
		db: utils.DB,
	}
}

func (rm *relationManager) SearchFriends(userId uint) ([]models.User, error) {
	relations := make([]models.Relation, 0)
	objIds := make([]uint, 0)
	err := utils.DB.Where("owner_id = ? and type = ?", userId, models.FriendRelation).Find(&relations).Error
	if err != nil {
		return nil, err
	}
	for _, v := range relations {
		objIds = append(objIds, v.TargetId)
	}
	users := make([]models.User, 0)
	err = utils.DB.Where("id in ?", objIds).Find(&users).Error
	if err != nil {
		return nil, err
	}
	return users, nil
}

// 添加好友
func (rm *relationManager) AddFriendByPhone(userId uint, targetPhone string) error {
	if targetPhone == "" {
		return errors.New("目标电话为空")
	}
	targetUser, err := Umanager.GetUserByPhone(targetPhone)
	if err != nil {
		return err
	}
	if targetUser.ID == userId {
		return errors.New("不可添加自己为好友")
	}
	// 添加两条记录 好友是双向的 互相可以调整黑名单
	relations := make([]models.Relation, 2)
	relations[0] = models.Relation{
		OwnerId:  userId,
		TargetId: targetUser.ID,
		Type:     models.FriendRelation,
	}
	relations[1] = models.Relation{
		OwnerId:  targetUser.ID,
		TargetId: userId,
		Type:     models.FriendRelation,
	}
	return rm.db.CreateInBatches(&relations, 2).Error
}
