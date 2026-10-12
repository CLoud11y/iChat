package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMQAdminExplicitSettings(t *testing.T) {
	file := filepath.Join(t.TempDir(), "local.env")
	if err := os.WriteFile(file, []byte("RABBITMQ_ADMIN_PASSWORD='admin:$ @中文'\nRABBITMQ_APP_PASSWORD='app:/?#'\nRABBITMQ_PORT=5673\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := loadSettings(file)
	if err != nil || s.adminPassword != "admin:$ @中文" || s.appPassword != "app:/?#" || s.amqpPort != "5673" || s.managementPort != "15672" {
		t.Fatalf("explicit settings failed: %v", err)
	}
	t.Setenv("RABBITMQ_PORT", "5674")
	s, err = loadSettings(file)
	if err != nil || s.amqpPort != "5674" {
		t.Fatal("environment override failed", err)
	}
	t.Setenv("RABBITMQ_ADMIN_PASSWORD", "")
	if _, err := loadSettings(file); err == nil {
		t.Fatal("empty override adopted file password")
	}
}

func TestMQAdminRejectsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{nil, {"delete"}, {"init", "--unknown=secret"}, {"check", "extra"}, {"check", "--env-file", filepath.Join(t.TempDir(), "missing.env")}} {
		if err := run(args, new(bytes.Buffer)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid command accepted or leaked input")
		}
	}
	t.Setenv("RABBITMQ_ADMIN_PASSWORD", "secret")
	for _, port := range []string{"0", "65536", "not-a-port", ""} {
		t.Setenv("RABBITMQ_PORT", port)
		if _, err := loadSettings(""); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid port accepted or leaked password")
		}
	}
}
