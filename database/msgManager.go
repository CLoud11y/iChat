package database

import (
	"context"
	"encoding/json"
	"errors"
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

func NewMessageManager(db *gorm.DB, rds *redis.Client) *msgManager {
	return &msgManager{
		rds: rds,
		db:  db,
	}
}

func (mm *msgManager) GetAllDirtyMsgs(ctx context.Context) ([]string, int, error) {
	if mm == nil || mm.rds == nil {
		return nil, 0, ErrStorageUnavailable
	}
	key := getDirtyMsgsKey()
	size, err := mm.rds.LLen(ctx, key).Result()
	if err != nil {
		return nil, 0, storageError("read dirty queue length", err)
	}
	if size == 0 {
		return nil, 0, nil
	}
	msgs, err := mm.rds.LRange(ctx, key, 0, size-1).Result()
	if err != nil {
		return nil, 0, storageError("read dirty queue", err)
	}
	return msgs, len(msgs), nil
}

func (mm *msgManager) SaveMsgs2DB(ctx context.Context, msgs []string) error {
	if mm == nil || mm.db == nil || mm.db.Config == nil {
		return ErrStorageUnavailable
	}
	if len(msgs) == 0 {
		return nil
	}
	// 先解析消息 []string -> []models.Message
	messages := make([]models.Message, len(msgs))
	var err error
	for i, msg := range msgs {
		err = json.Unmarshal([]byte(msg), &messages[i])
		if err != nil {
			return fmt.Errorf("decode dirty message %d: %w", i, err)
		}
	}
	// 存入数据库
	if err := mm.db.WithContext(ctx).CreateInBatches(messages, 128).Error; err != nil {
		return storageError("persist dirty messages", err)
	}
	return nil
}

func (mm *msgManager) RemDirtyMsgs(ctx context.Context, size int) error {
	if mm == nil || mm.rds == nil {
		return ErrStorageUnavailable
	}
	if size <= 0 {
		return fmt.Errorf("dirty message acknowledgement count must be positive")
	}
	if err := mm.rds.LTrim(ctx, getDirtyMsgsKey(), int64(size), -1).Err(); err != nil {
		return storageError("acknowledge dirty messages", err)
	}
	return nil
}

func (mm *msgManager) LoadMsgs(ctx context.Context, uIdA, uIdB, msgType uint, earliestMsg models.Message, cnt int) ([]string, error) {
	if uIdA == 0 || uIdB == 0 || (msgType != models.PrivateType && msgType != models.GroupType) || cnt < 1 || cnt > MaxMessagePageSize {
		return nil, ErrInvalidMessage
	}
	if mm == nil || mm.rds == nil {
		return nil, ErrStorageUnavailable
	}
	rows, err := mm.loadMsgs(ctx, uIdA, uIdB, msgType, earliestMsg, cnt)
	if err != nil && !errors.Is(err, ErrMessageCursorNotFound) {
		return nil, storageError("load message history", err)
	}
	return rows, err
}

func (mm *msgManager) loadMsgs(ctx context.Context, uIdA, uIdB, msgType uint, earliestMsg models.Message, cnt int) ([]string, error) {
	key := getKey(&models.Message{SenderId: uIdA, ReceiverId: uIdB, Type: msgType})
	// 检查key是否存在 若不存在则去数据库获取数据并存入redis
	n, err := mm.rds.Exists(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		migrated, migrateErr := mm.migrateLegacyMsgs(ctx, uIdA, uIdB, msgType)
		if migrateErr != nil {
			return nil, migrateErr
		}
		if !migrated {
			err = mm.syncMsgsFromDB2Rds(ctx, uIdA, uIdB, msgType)
		}
		if err != nil {
			return nil, err
		}
	}
	start := int64(0)
	if earliestMsg.Type != models.InvalidType {
		member, encodeErr := encodeCachedMessage(&earliestMsg)
		if encodeErr != nil {
			return nil, encodeErr
		}
		start, err = mm.rds.ZRevRank(ctx, key, member).Result()
		if errors.Is(err, redis.Nil) {
			return nil, ErrMessageCursorNotFound
		}
		if err != nil {
			return nil, err
		}
		start++ // 排除掉earliestMsg
	}
	cachedMessages, err := mm.rds.ZRevRange(ctx, key, start, start+int64(cnt-1)).Result()
	if err != nil {
		return nil, err
	}
	return decodeCachedMessages(cachedMessages)
}

// 保存消息到redis 同时标记为脏数据
func (mm *msgManager) SaveMsg(ctx context.Context, msg *models.Message) error {
	if !validMessage(msg) {
		return ErrInvalidMessage
	}
	if mm == nil || mm.rds == nil {
		return ErrStorageUnavailable
	}
	key := getKey(msg)
	member, err := encodeCachedMessage(msg)
	if err != nil {
		return err
	}
	_, err = mm.rds.ZAdd(ctx, key, &redis.Z{
		Score:  getScore(msg),
		Member: member,
	}).Result()
	if err != nil {
		return storageError("cache message", err)
	}
	// 记录脏数据
	if err := mm.rds.RPush(ctx, getDirtyMsgsKey(), msg).Err(); err != nil {
		return storageError("enqueue dirty message", err)
	}
	return nil
}

func (mm *msgManager) syncMsgsFromDB2Rds(ctx context.Context, uIdA, uIdB, msgType uint) error {
	if mm == nil || mm.db == nil || mm.db.Config == nil {
		return ErrStorageUnavailable
	}
	// 从数据库获取
	var messages []models.Message
	var err error
	switch msgType {
	case models.GroupType:
		err = mm.db.WithContext(ctx).Where("receiver_id =? AND type =?", uIdB, msgType).Find(&messages).Error
		if err != nil {
			return err
		}
	case models.PrivateType:
		err = mm.db.WithContext(ctx).Where("sender_id =? AND receiver_id =? AND type =?", uIdA, uIdB, msgType).Find(&messages).Error
		if err != nil {
			return err
		}
		var tempMsgs []models.Message
		err = mm.db.WithContext(ctx).Where("sender_id =? AND receiver_id =? AND type =?", uIdB, uIdA, msgType).Find(&tempMsgs).Error
		if err != nil {
			return err
		}
		messages = append(messages, tempMsgs...)
	}

	// 存入redis
	return mm.saveMsgs(ctx, messages)
}

func (mm *msgManager) migrateLegacyMsgs(ctx context.Context, uIdA, uIdB, msgType uint) (bool, error) {
	legacyKey := getLegacyKey(&models.Message{SenderId: uIdA, ReceiverId: uIdB, Type: msgType})
	cachedMessages, err := mm.rds.ZRange(ctx, legacyKey, 0, -1).Result()
	if err != nil {
		return false, err
	}
	if len(cachedMessages) == 0 {
		return false, nil
	}

	messages := make([]models.Message, len(cachedMessages))
	for i, cachedMessage := range cachedMessages {
		if err := json.Unmarshal([]byte(cachedMessage), &messages[i]); err != nil {
			return false, err
		}
	}
	if err := mm.saveMsgs(ctx, messages); err != nil {
		return false, err
	}
	return true, nil
}

// 批量保存消息到redis 不标记为脏数据
func (mm *msgManager) saveMsgs(ctx context.Context, msgs []models.Message) error {
	for _, msg := range msgs {
		if !validMessage(&msg) {
			return fmt.Errorf("invalid cached message")
		}
		key := getKey(&msg)
		member, err := encodeCachedMessage(&msg)
		if err != nil {
			return err
		}
		_, err = mm.rds.ZAdd(ctx, key, &redis.Z{
			Score:  getScore(&msg),
			Member: member,
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
func (mm *msgManager) PublishAndSave(ctx context.Context, msg *models.Message) error {
	// save
	err := mm.SaveMsg(ctx, msg)
	if err != nil {
		return err
	}
	// publish 若失败需要删掉redis中存的内容
	err = mm.PublishMsg(ctx, msg)
	if err != nil {
		member, encodeErr := encodeCachedMessage(msg)
		if encodeErr == nil {
			mm.rds.ZRem(ctx, getKey(msg), member)
		}
		return err
	}
	return nil
}

func (mm *msgManager) PublishMsg(ctx context.Context, msg *models.Message) error {
	if !validMessage(msg) {
		return ErrInvalidMessage
	}
	if mm == nil || mm.rds == nil {
		return ErrStorageUnavailable
	}
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
		return fmt.Errorf("unknown message type: %d", msg.Type)
	}
	if err := mm.rds.Publish(ctx, receiverChannel, p).Err(); err != nil {
		return storageError("publish message", err)
	}
	return nil
}

func validMessage(msg *models.Message) bool {
	return msg != nil && msg.SenderId != 0 && msg.ReceiverId != 0 && (msg.Type == models.PrivateType || msg.Type == models.GroupType)
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
	groupIds, err := Gmanager.GetGroupIds(context.Background(), uid)
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

// getScore 仅使用毫秒时间戳。当前时间戳小于 2^53，可由 float64 精确表示。
// 同一毫秒内的消息通过 ZSET member 的固定宽度前缀按 identifier 排序。
func getScore(msg *models.Message) float64 {
	return float64(msg.TimeStamp)
}

const cachedMessagePrefixLength = 42

func encodeCachedMessage(msg *models.Message) (string, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%020d:%020d:%s", msg.TimeStamp, msg.Identifier, payload), nil
}

func decodeCachedMessages(cachedMessages []string) ([]string, error) {
	messages := make([]string, len(cachedMessages))
	for i, cachedMessage := range cachedMessages {
		if len(cachedMessage) < cachedMessagePrefixLength ||
			cachedMessage[20] != ':' ||
			cachedMessage[41] != ':' {
			return nil, fmt.Errorf("invalid cached message format")
		}
		messages[i] = cachedMessage[cachedMessagePrefixLength:]
	}
	return messages, nil
}

// key 格式为 msg:private:uidA.uidB (uidA<uidB)或 msg:group:groupId
func getKey(msg *models.Message) string {
	return getMessageKey("msg:v2:", msg)
}

func getLegacyKey(msg *models.Message) string {
	return getMessageKey("msg:", msg)
}

func getMessageKey(prefix string, msg *models.Message) string {
	sender, receiver := strconv.FormatUint(uint64(msg.SenderId), 10), strconv.FormatUint(uint64(msg.ReceiverId), 10)
	key := prefix
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
		utils.Logger().WithField("message_type", msg.Type).Panic("unknown message type")
	}
	return key
}
