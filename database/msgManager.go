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

// GetAllDirtyMsgs retains the legacy API name but returns at most one bounded
// batch. The count is derived from the records actually read, never from LLEN.
func (mm *msgManager) GetAllDirtyMsgs(ctx context.Context) ([]string, int, error) {
	if mm == nil || mm.rds == nil {
		return nil, 0, ErrStorageUnavailable
	}
	msgs, err := mm.rds.LRange(ctx, getDirtyMsgsKey(), 0, DirtyMessageBatchSize-1).Result()
	if err != nil {
		return nil, 0, storageError("read dirty queue", err)
	}
	return msgs, len(msgs), nil
}

func (mm *msgManager) SaveMsgs2DB(ctx context.Context, msgs []string) error {
	if mm == nil || mm.db == nil || mm.db.Config == nil {
		return ErrStorageUnavailable
	}
	if err := ctx.Err(); err != nil {
		return storageError("persist dirty messages", err)
	}
	if len(msgs) == 0 {
		return nil
	}
	messages := make([]models.Message, len(msgs))
	for i, raw := range msgs {
		if err := json.Unmarshal([]byte(raw), &messages[i]); err != nil {
			return fmt.Errorf("decode dirty message %d: %w", i, err)
		}
		if messages[i].Id == "" {
			return fmt.Errorf("dirty message %d: %w: missing ID", i, ErrInvalidMessage)
		}
	}
	// Direct callers require whole-batch success. The worker uses ProcessDirtyMsgs
	// for per-record isolation and durable prefix progress.
	err := mm.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range messages {
			if err := persistLegacyMessage(tx, &messages[i]); err != nil {
				return fmt.Errorf("dirty message %d: %w", i, err)
			}
		}
		return nil
	})
	if errors.Is(err, ErrMessageConflict) {
		return err
	}
	if err != nil {
		return storageError("persist dirty messages", err)
	}
	return nil
}

// RemDirtyMsgs acknowledges only a committed (or durably isolated) raw prefix.
// Comparing and trimming happen in a single Redis execution; mismatches do not
// mutate the queue. A Redis error does not prove the script did not execute.
func (mm *msgManager) RemDirtyMsgs(ctx context.Context, prefix []string) error {
	if mm == nil || mm.rds == nil {
		return ErrStorageUnavailable
	}
	if len(prefix) == 0 || len(prefix) > DirtyMessageBatchSize {
		return fmt.Errorf("dirty message acknowledgement size must be between 1 and %d", DirtyMessageBatchSize)
	}
	args := make([]interface{}, len(prefix))
	for i, raw := range prefix {
		args[i] = raw
	}
	result, err := mm.rds.Eval(ctx, acknowledgeDirtyPrefix, []string{getDirtyMsgsKey()}, args...).Int64()
	if err != nil {
		return storageError("acknowledge dirty messages", err)
	}
	if result == 0 {
		return ErrDirtyMessagePrefixMismatch
	}
	if result != 1 {
		return fmt.Errorf("unexpected dirty message acknowledgement result: %d", result)
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
	if err := ctx.Err(); err != nil {
		return storageError("enqueue dirty message", err)
	}
	member, err := encodeCachedMessage(msg)
	if err != nil {
		return err
	}
	// Once RPUSH starts, an error cannot prove that no attempt was accepted.
	// The shared Redis client may retry after losing an earlier reply.
	if err := mm.rds.RPush(ctx, getDirtyMsgsKey(), msg).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrMessageEnqueueUnknown, storageError("enqueue dirty message", err))
	}
	// A confirmed enqueue owns the persistence responsibility. Cache failures
	// cannot undo it or turn the accepted send into a definite failure.
	if err := mm.rds.ZAdd(ctx, getKey(msg), &redis.Z{
		Score: getScore(msg), Member: member,
	}).Err(); err != nil {
		utils.Logger().WithError(err).WithField("message_id", msg.Id).
			WithField("sender_id", msg.SenderId).Warn("legacy message enqueued; cache update failed")
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

// PublishAndSave confirms the legacy enqueue before attempting notification.
func (mm *msgManager) PublishAndSave(ctx context.Context, msg *models.Message) error {
	if err := mm.SaveMsg(ctx, msg); err != nil {
		return err
	}
	if err := mm.PublishMsg(ctx, msg); err != nil {
		utils.Logger().WithError(err).WithField("message_id", msg.Id).
			WithField("sender_id", msg.SenderId).Warn("legacy message enqueued; notification failed")
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
