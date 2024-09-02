package models

import "gorm.io/gorm"

const (
	FriendRelation = iota
	GroupRelation
)

//人员关系
type Relation struct {
	gorm.Model
	OwnerId  uint `gorm:"uniqueIndex:owner_id;"` //与targetId和type构成复合索引
	TargetId uint `gorm:"uniqueIndex:owner_id;"` //对应人/群 ID
	Type     uint `gorm:"uniqueIndex:owner_id;"` //关系类型
	Desc     string
}

func (Relation) TableName() string {
	return "relation"
}
