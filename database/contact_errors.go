package database

import (
	"errors"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

var (
	ErrEmptyFriendPhone = errors.New("目标电话为空")
	ErrSelfFriend       = errors.New("不可添加自己为好友")
	ErrRelationExists   = errors.New("关系已存在")
	ErrGroupNotOwned    = errors.New("群不存在或无权删除")
)

func relationWriteError(operation string, err error) error {
	var duplicate *mysqlDriver.MySQLError
	if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &duplicate) && duplicate.Number == 1062) {
		return ErrRelationExists
	}
	return storageError(operation, err)
}
