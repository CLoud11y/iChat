package database

import (
	"errors"
	"fmt"
	"iChat/models"
	"iChat/utils"
	"sync"

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

func init() {
	Umanager = &userManager{
		db:        utils.DB,
		onlineMap: make(map[uint]*models.User),
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

func (um *userManager) Online(user *models.User) {
	um.locker.Lock()
	defer um.locker.Unlock()
	um.onlineMap[user.ID] = user
}

func (um *userManager) Offline(uid uint) {
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
	fmt.Println(uid, um.onlineMap)
	if user, ok := um.onlineMap[uid]; ok {
		return user.Ws
	}
	return nil
}
