package models

import (
	"gorm.io/gorm"
)

type Group struct {
	gorm.Model
	Name    string `gorm:"not null; type: varchar(32);" json:"name"`
	OwnerId uint   `json:"ownerId"`
	// UserIds Uids   `json:"userIds"`
	MaxUser uint   `json:"maxUser"` // 最大用户数
	Desc    string `json:"desc"`
}

func (Group) TableName() string {
	return "group"
}

// type Uids []uint

// func (t *Uids) Scan(value interface{}) error {
// 	bytesValue, _ := value.([]byte)
// 	return json.Unmarshal(bytesValue, t)
// }

// func (t Uids) Value() (driver.Value, error) {
// 	return json.Marshal(t)
// }
