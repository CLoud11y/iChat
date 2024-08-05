package models

import (
	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Identifier string
	Name       string
	Password   string
	Phone      string
	Email      string
	ClientIp   string
	ClientPort string
	// LoginTime     time.Time
	// LogoutTime    time.Time
	// HeartBeatTime time.Time
	isLogout   bool
	DeviceInfo string
}

func (User) TableName() string {
	return "user_basic"
}
