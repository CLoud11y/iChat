//go:build integration

package database

import (
	"context"
	"iChat/models"
	"strconv"
	"testing"
	"time"
)

func TestSaveAndLoadMsg(t *testing.T) {
	t.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
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
	t.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
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
	b.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Gmanager.GetGroupsByUid(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetGroups2(b *testing.B) {
	b.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Gmanager.GetGroupsByUid2(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestGetGroups2(t *testing.T) {
	t.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
	g, err := Gmanager.GetGroupsByUid2(9)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(g)
}

func TestSearchFriends(t *testing.T) {
	t.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
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
	b.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Rmanager.SearchFriends(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchFriends2(b *testing.B) {
	b.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Rmanager.SearchFriends2(9)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestGetGroupUsers(t *testing.T) {
	t.Skip("P0: legacy integration test disabled until isolated fixtures replace fixed IDs")
	users, err := Gmanager.GetGroupUsers(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range users {
		t.Log(v.Name)
	}
}
