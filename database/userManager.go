package database

import (
	"context"
	"errors"
	"fmt"
	"iChat/models"
	"sync"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

var Umanager *userManager

type userManager struct {
	db *gorm.DB
	// 存放所有在线用户的map
	locker    sync.RWMutex
	onlineMap map[uint]*models.User
}

func NewUserManager(db *gorm.DB) *userManager {
	return &userManager{
		db:        db,
		onlineMap: make(map[uint]*models.User),
	}
}

func (um *userManager) Signup(ctx context.Context, phone, name, password string) (*models.User, error) {
	if um == nil || um.db == nil || um.db.Config == nil {
		return nil, ErrStorageUnavailable
	}
	db := um.db.WithContext(ctx)
	// 检查用户名是否已存在
	tempUser := &models.User{}
	err := db.Where("phone = ?", phone).First(tempUser).Error
	// 用户已存在
	if err == nil {
		return nil, ErrPhoneRegistered
	}
	// 发生未知错误
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, storageError("check registered phone", err)
	}
	// 正常注册流程
	tempUser.Name = name
	tempUser.Password = password
	tempUser.Phone = phone
	// 默认将标识号设置为phone
	tempUser.Identifier = phone
	err = db.Create(tempUser).Error
	if err != nil {
		// The unique constraint is authoritative if concurrent signups both
		// observe an absent phone before either insert commits.
		var duplicate *mysqlDriver.MySQLError
		if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &duplicate) && duplicate.Number == 1062) {
			return nil, ErrPhoneRegistered
		}
		return nil, storageError("create user", err)
	}
	return tempUser, nil
}

func (um *userManager) GetUserByPhone(ctx context.Context, phone string) (*models.User, error) {
	if um == nil || um.db == nil || um.db.Config == nil {
		return nil, ErrStorageUnavailable
	}
	tempUser := &models.User{}
	err := um.db.WithContext(ctx).Where("phone = ?", phone).First(tempUser).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %w", ErrUserNotFound, err)
	}
	if err != nil {
		return nil, storageError("find user by phone", err)
	}
	return tempUser, nil
}

func (um *userManager) Online(user *models.User) {
	um.locker.Lock()
	defer um.locker.Unlock()
	um.onlineMap[user.ID] = user
}

func (um *userManager) Offline(uid uint) {
	if um == nil {
		return
	}
	um.locker.Lock()
	defer um.locker.Unlock()
	delete(um.onlineMap, uid)
}

func (um *userManager) UpdateWs(uid uint, ws *websocket.Conn) {
	um.locker.Lock()
	defer um.locker.Unlock()
	if user, ok := um.onlineMap[uid]; ok {
		user.Ws = ws
	}
}

func (um *userManager) GetOnlineUserWs(uid uint) *websocket.Conn {
	um.locker.RLock()
	defer um.locker.RUnlock()
	if user, ok := um.onlineMap[uid]; ok {
		return user.Ws
	}
	return nil
}
