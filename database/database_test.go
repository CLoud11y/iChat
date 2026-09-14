package database

import (
	"encoding/json"
	"iChat/models"
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
