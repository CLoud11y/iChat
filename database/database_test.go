package database

import (
	"context"
	"encoding/json"
	"iChat/models"
	"strconv"
	"testing"
	"time"
)

func TestGetScore(t *testing.T) {
	timestamp := time.Now().UnixMilli()
	first := &models.Message{TimeStamp: timestamp, Identifier: 1, Content: "first"}
	second := &models.Message{TimeStamp: timestamp, Identifier: 2, Content: "second"}

	if got := getScore(first); got != float64(timestamp) {
		t.Fatalf("score = %v, want exact timestamp %d", got, timestamp)
	}
	if getScore(&models.Message{TimeStamp: timestamp + 1}) <= getScore(first) {
		t.Fatal("a later timestamp must have a greater score")
	}

	firstMember, err := encodeCachedMessage(first)
	if err != nil {
		t.Fatal(err)
	}
	secondMember, err := encodeCachedMessage(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstMember >= secondMember {
		t.Fatal("members in the same millisecond must sort by identifier")
	}

	decoded, err := decodeCachedMessages([]string{secondMember, firstMember})
	if err != nil {
		t.Fatal(err)
	}
	var got models.Message
	if err := json.Unmarshal([]byte(decoded[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Identifier != second.Identifier || got.Content != second.Content {
		t.Fatalf("decoded message = %#v, want %#v", got, *second)
	}
}

func TestSaveAndLoadMsg(t *testing.T) {
	round := 10
	uIdA, uIdB := uint(1), uint(2)
	var err error
	// save
	for i := 0; i < round; i++ {
		msg1 := &models.Message{
			SenderId:   uIdA,
			ReceiverId: uIdB,
			Type:       models.PrivateType,
			Content:    strconv.Itoa(i),
			TimeStamp:  time.Now().UnixMilli(),
			Identifier: uint(i),
		}
		msg2 := &models.Message{
			SenderId:   uIdB,
			ReceiverId: uIdA,
			Type:       models.PrivateType,
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
	strMsgs, err := Mmanager.LoadMsgs(uIdA, uIdB, models.PrivateType, models.Message{Type: models.InvalidType}, round)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range strMsgs {
		t.Log(i, v)
	}
	// delete test msgs
	_, err = Mmanager.rds.Del(context.Background(), getKey(&models.Message{SenderId: uIdA, ReceiverId: uIdB, Type: models.PrivateType})).Result()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteMsg(t *testing.T) {
	uIdA, uIdB := uint(18), uint(9)
	_, err := Mmanager.rds.Del(context.Background(), getKey(&models.Message{SenderId: uIdA, ReceiverId: uIdB, Type: models.PrivateType})).Result()
	if err != nil {
		t.Fatal(err)
	}
	strMsgs, err := Mmanager.rds.ZRange(context.Background(), getKey(&models.Message{SenderId: uIdA, ReceiverId: uIdB, Type: models.PrivateType}), 0, -1).Result()
	if err != nil || len(strMsgs) != 0 {
		t.Fatal("delete msg failed: ", err)
	}
}

func BenchmarkGetGroups(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Gmanager.GetGroupsByUid(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetGroups2(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Gmanager.GetGroupsByUid2(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestGetGroups2(t *testing.T) {
	g, err := Gmanager.GetGroupsByUid2(9)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(g)
}

func TestSearchFriends(t *testing.T) {
	friends, err := Rmanager.SearchFriends(9)
	if err != nil {
		t.Fatal(err)
	}
	friends2, err := Rmanager.SearchFriends2(9)
	if err != nil {
		t.Fatal(err)
	}
	if len(friends) != len(friends2) {
		t.Fatal("search friends failed", friends, friends2)
	}
	for i, v := range friends {
		if v.ID != friends2[i].ID {
			t.Fatal("search friends failed")
		}
	}
}

func BenchmarkSearchFriends(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Rmanager.SearchFriends(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchFriends2(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Rmanager.SearchFriends2(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestGetGroupUsers(t *testing.T) {
	users, err := Gmanager.GetGroupUsers(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range users {
		t.Log(v.Name)
	}
}
