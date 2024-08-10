package database

import (
	"errors"
	"iChat/models"
	"iChat/utils"

	"gorm.io/gorm"
)

var UserManager *userManager

type userManager struct {
	db *gorm.DB
}

func init() {
	UserManager = &userManager{
		db: utils.DB,
	}
}

func (um *userManager) Signup(phone, name, password string) (*models.User, error) {
	// 检查用户名是否已存在
	tempUser := &models.User{}
	err := um.db.Where("phone = ?", phone).First(tempUser).Error
	// 用户已存在
	if err == nil {
		return nil, errors.New("phone is already registered")
	}
	// 发生未知错误
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	// 正常注册流程
	tempUser.Name = name
	tempUser.Password = password
	tempUser.Phone = phone
	// 默认将标识号设置为phone
	tempUser.Identifier = phone
	err = um.db.Save(&tempUser).Error
	if err != nil {
		return nil, err
	}
	return tempUser, nil
}

func (um *userManager) GetUserByPhone(phone string) (*models.User, error) {
	tempUser := &models.User{}
	err := um.db.Where("phone = ?", phone).First(tempUser).Error
	return tempUser, err
}
