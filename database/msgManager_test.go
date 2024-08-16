package database

import (
	"context"
	"iChat/models"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

// 每条消息间隔0-10ms进行测试
func TestGetScore(t *testing.T) {
	round := 1024
	preScore, score := 0.0, 0.0
	var preMsg, msg *models.Message
	rand := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < round; i++ {
		msg = &models.Message{TimeStamp: time.Now().UnixMilli(), Identifier: uint(i)}
		score = getScore(msg)
		if score <= preScore {
			t.Error("getScore 计算有误 新消息score小于旧消息")
			t.Error(msg.TimeStamp, msg.Identifier, score)
			t.Error(preMsg.TimeStamp, preMsg.Identifier, preScore)
			t.FailNow()
		}
		preScore = score
		preMsg = msg
		r := rand.Intn(10)
		time.Sleep(time.Duration(r) * time.Millisecond)
	}
}

func TestSaveAndLoadMsg(t *testing.T) {
	round := 10
	uIdA, uIdB := uint(1), uint(2)
	var err error
	// save
	for i := 0; i < round; i++ {
		msg1 := &models.Message{SenderId: uIdA,
			ReceiverId: uIdB,
			Content:    strconv.Itoa(i),
			TimeStamp:  time.Now().UnixMilli(),
			Identifier: uint(i),
		}
		msg2 := &models.Message{
			SenderId:   uIdB,
			ReceiverId: uIdA,
			Content:    strconv.Itoa(i),
			TimeStamp:  time.Now().UnixMilli(),
			Identifier: uint(i),
		}
		err = Mmanager.SaveMsg(msg1)
		if err != nil {
			t.Fatal(err)
		}
		err = Mmanager.SaveMsg(msg2)
		if err != nil {
			t.Fatal(err)
		}
	}
	// load
	strMsgs, err := Mmanager.LoadMsgs(uIdA, uIdB, 0, round)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range strMsgs {
		t.Log(i, v)
	}
	// delete test msgs
	_, err = Mmanager.rds.Del(context.Background(), getKey(uIdA, uIdB)).Result()
	if err != nil {
		t.Fatal(err)
	}
}
