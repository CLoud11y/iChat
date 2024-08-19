package database

import (
	"context"
	"encoding/json"
	"iChat/models"
	"iChat/utils"
	"strconv"

	"github.com/go-redis/redis/v8"
)

var Mmanager *msgManager

type msgManager struct {
	rds *redis.Client
}

func init() {
	Mmanager = &msgManager{
		rds: utils.RDS,
	}
}

func (mm *msgManager) LoadMsgs(uIdA, uIdB uint, earliestMsg models.Message, cnt int) ([]string, error) {
	key := getKey(uIdA, uIdB)
	ctx := context.Background()
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

func (mm *msgManager) SaveMsg(msg *models.Message) error {
	key := getKey(msg.SenderId, msg.ReceiverId)
	_, err := mm.rds.ZAdd(context.Background(), key, &redis.Z{
		Score:  getScore(msg),
		Member: msg,
	}).Result()
	return err
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
		mm.rds.ZRem(context.Background(), getKey(msg.SenderId, msg.ReceiverId), msg)
		return err
	}
	return nil
}

func (mm *msgManager) PublishMsg(msg *models.Message) error {
	p, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	receiver := strconv.FormatUint(uint64(msg.ReceiverId), 10)
	_, err = mm.rds.Publish(context.Background(), receiver, p).Result()
	return err
}

func (mm *msgManager) Subscribe(uid uint) (<-chan *redis.Message, error) {
	channel := strconv.FormatUint(uint64(uid), 10)
	sub := mm.rds.Subscribe(context.Background(), channel)
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

// key 格式为 msg_uidA_uidB (uidA<uidB)
func getKey(uIdA, uIdB uint) string {
	sender, receiver := strconv.FormatUint(uint64(uIdA), 10), strconv.FormatUint(uint64(uIdB), 10)
	key := "msg_"
	if uIdA < uIdB {
		key += sender + "_" + receiver
	} else {
		key += receiver + "_" + sender
	}
	return key
}
