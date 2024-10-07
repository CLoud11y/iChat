package database

import (
	"context"
	"encoding/json"
	"fmt"
	"iChat/models"
	"iChat/utils"
	"strconv"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

var Mmanager *msgManager

type msgManager struct {
	rds *redis.Client
	db  *gorm.DB
}

func init() {
	Mmanager = &msgManager{
		rds: utils.RDS,
		db:  utils.DB,
	}
}

func (mm *msgManager) GetAllDirtyMsgs() ([]string, int, error) {
	key := getDirtyMsgsKey()
	ctx := context.Background()
	size := mm.rds.LLen(ctx, key).Val()
	if size == 0 {
		return nil, 0, nil
	}
	msgs, err := mm.rds.LRange(ctx, key, 0, size).Result()
	if err != nil {
		return nil, 0, err
	}
	return msgs, int(size), nil
}

func (mm *msgManager) SaveMsgs2DB(msgs []string) error {
	// 先解析消息 []string -> []models.Message
	messages := make([]models.Message, len(msgs))
	var err error
	for i, msg := range msgs {
		err = json.Unmarshal([]byte(msg), &messages[i])
		if err != nil {
			return err
		}
	}
	// 存入数据库
	return mm.db.CreateInBatches(messages, 128).Error
}

func (mm *msgManager) RemDirtyMsgs(size int) error {
	return mm.rds.LTrim(context.Background(), getDirtyMsgsKey(), int64(size), -1).Err()
}

func (mm *msgManager) LoadMsgs(uIdA, uIdB, msgType uint, earliestMsg models.Message, cnt int) ([]string, error) {
	key := getKey(&models.Message{SenderId: uIdA, ReceiverId: uIdB, Type: msgType})
	ctx := context.Background()
	// 检查key是否存在 若不存在则去数据库获取数据并存入redis
	n, err := mm.rds.Exists(context.Background(), key).Result()
	if err != nil {
		return nil, err
	}
	fmt.Println(1)
	if n == 0 {
		fmt.Println(2)
		err = mm.syncMsgsFromDB2Rds(uIdA, uIdB, msgType)
		if err != nil {
			return nil, err
		}
	}
	fmt.Println(3)
	// 最近的消息
	p, err := json.Marshal(earliestMsg)
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if earliestMsg.Type != models.InvalidType {
		start, err = mm.rds.ZRevRank(ctx, key, string(p)).Result()
		if err != nil {
			return nil, err
		}
		start++ // 排除掉earliestMsg
	}
	return mm.rds.ZRevRange(ctx, key, start, start+int64(cnt-1)).Result()
}

// 保存消息到redis 同时标记为脏数据
func (mm *msgManager) SaveMsg(msg *models.Message) error {
	ctx := context.Background()
	key := getKey(msg)
	_, err := mm.rds.ZAdd(ctx, key, &redis.Z{
		Score:  getScore(msg),
		Member: msg,
	}).Result()
	if err != nil {
		return err
	}
	// 记录脏数据
	mm.rds.RPush(ctx, getDirtyMsgsKey(), msg)
	return err
}

func (mm *msgManager) syncMsgsFromDB2Rds(uIdA, uIdB, msgType uint) error {
	// 从数据库获取
	var messages []models.Message
	var err error
	switch msgType {
	case models.GroupType:
		err = mm.db.Where("receiver_id =? AND type =?", uIdB, msgType).Find(&messages).Error
		if err != nil {
			return err
		}
	case models.PrivateType:
		err = mm.db.Where("sender_id =? AND receiver_id =? AND type =?", uIdA, uIdB, msgType).Find(&messages).Error
		if err != nil {
			return err
		}
		var tempMsgs []models.Message
		err = mm.db.Where("sender_id =? AND receiver_id =? AND type =?", uIdB, uIdA, msgType).Find(&tempMsgs).Error
		if err != nil {
			return err
		}
		messages = append(messages, tempMsgs...)
	}

	// 存入redis
	return mm.saveMsgs(messages)
}

// 批量保存消息到redis 不标记为脏数据
func (mm *msgManager) saveMsgs(msgs []models.Message) error {
	ctx := context.Background()
	for _, msg := range msgs {
		key := getKey(&msg)
		_, err := mm.rds.ZAdd(ctx, key, &redis.Z{
			Score:  getScore(&msg),
			Member: msg,
		}).Result()
		if err != nil {
			return err
		}
	}
	return nil
}

func getDirtyMsgsKey() string {
	return "dirty:msgs"
}

// 先存还是先publish呢？？？
func (mm *msgManager) PublishAndSave(msg *models.Message) error {
	// save
	err := mm.SaveMsg(msg)
	if err != nil {
		return err
	}
	// publish 若失败需要删掉redis中存的内容
	err = mm.PublishMsg(msg)
	if err != nil {
		mm.rds.ZRem(context.Background(), getKey(msg), msg)
		return err
	}
	return nil
}

func (mm *msgManager) PublishMsg(msg *models.Message) error {
	p, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	receiverChannel := ""
	switch msg.Type {
	case models.GroupType:
		receiverChannel = getGroupChannel(msg.ReceiverId)
	case models.PrivateType:
		receiverChannel = getPrivateChannel(msg.ReceiverId)
	default:
		fmt.Println("unknown msg type")
	}
	_, err = mm.rds.Publish(context.Background(), receiverChannel, p).Result()
	return err
}

func getGroupChannel(groupId uint) string {
	return "group_" + strconv.FormatUint(uint64(groupId), 10)
}

func getPrivateChannel(userId uint) string {
	return "private_" + strconv.FormatUint(uint64(userId), 10)
}

func (mm *msgManager) Subscribe(uid uint) (<-chan *redis.Message, error) {
	channel := getPrivateChannel(uid)
	sub := mm.rds.Subscribe(context.Background(), channel)
	return sub.Channel(), nil
}

func (mm *msgManager) SubscribeGroups(uid uint) (<-chan *redis.Message, error) {
	groupIds, err := Gmanager.GetGroupIds(uid)
	if err != nil {
		return nil, err
	}
	channels := make([]string, len(groupIds))
	for i, id := range groupIds {
		channels[i] = getGroupChannel(id)
	}
	sub := mm.rds.Subscribe(context.Background(), channels...)
	return sub.Channel(), nil
}

/*
根据msg的时间戳和id计算出score, redis用score排序
消息顺序首先考虑时间戳 时间戳越大消息越靠前 时间戳相同时考虑id的影响
float64精度为约15位有效数字，时间戳占了13位 则后两位可供id使用
故score计算方法为时间戳+(id%100)/100
注：当时间戳相同且(id%100)/100由99到100时会造成score逆序
*/
func getScore(msg *models.Message) (score float64) {
	score = float64(msg.TimeStamp) + float64(msg.Identifier%100)/100
	return
}

// key 格式为 msg:private:uidA.uidB (uidA<uidB)或 msg:group:groupId
func getKey(msg *models.Message) string {
	sender, receiver := strconv.FormatUint(uint64(msg.SenderId), 10), strconv.FormatUint(uint64(msg.ReceiverId), 10)
	key := "msg:"
	switch msg.Type {
	case models.GroupType:
		key += "group:" + receiver
	case models.PrivateType:
		key += "private:"
		if msg.SenderId < msg.ReceiverId {
			key += sender + "." + receiver
		} else {
			key += receiver + "." + sender
		}
	default:
		utils.Logger().Panicln("unknown msg type", msg.Type)
	}
	return key
}
