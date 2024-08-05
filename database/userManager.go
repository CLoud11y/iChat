package database

import (
	"errors"
	"fmt"
	"iChat/config"
	"iChat/models"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var UserManager *userManager

type userManager struct {
	db *gorm.DB
}

func init() {
	// 初始化数据库连接
	mysqlConf := &config.Conf.MYSQL
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		mysqlConf.User,
		mysqlConf.Password,
		mysqlConf.Host,
		mysqlConf.Port,
		mysqlConf.DbName,
	)
	fmt.Println("DSN:", dsn)

	MysqlConf := mysql.New(mysql.Config{DSN: dsn})
	GormConf := &gorm.Config{}

	DB, err := gorm.Open(MysqlConf, GormConf)
	if err != nil {
		panic(err)
	}
	DB.AutoMigrate(&models.User{})

	UserManager = &userManager{
		db: DB,
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
	err = um.db.Save(&tempUser).Error
	if err != nil {
		return nil, err
	}
	return tempUser, nil
}
