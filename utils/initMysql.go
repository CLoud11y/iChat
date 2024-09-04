package utils

import (
	"fmt"
	"iChat/config"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var DB *gorm.DB

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

	db, err := gorm.Open(MysqlConf, GormConf)
	if err != nil {
		panic(err)
	}
	// db.AutoMigrate(&models.User{})
	// db.AutoMigrate(&models.Relation{})
	// db.AutoMigrate(&models.Group{})
	// db.AutoMigrate(&models.Message{})
	DB = db
}
