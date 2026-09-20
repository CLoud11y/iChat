package database

import (
	"bufio"
	"context"
	"errors"
	"iChat/config"
	"iChat/models"
	"iChat/utils"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// A pipe peer records complete RESP commands and can lose an enqueue reply.
// No external Redis, development configuration, or TCP listener is used.
func acceptanceRedis(t *testing.T, fail string, loseReply bool) (*redis.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var commands []string
	var peers []net.Conn
	var wg sync.WaitGroup
	rds := redis.NewClient(&redis.Options{MaxRetries: -1, ReadTimeout: time.Second, WriteTimeout: time.Second,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			mu.Lock()
			peers = append(peers, server)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer server.Close()
				r := bufio.NewReader(server)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
					if err != nil || n < 1 {
						t.Errorf("invalid RESP array: %q", line)
						return
					}
					args := make([]string, n)
					for i := range args {
						line, err = r.ReadString('\n')
						if err != nil {
							return
						}
						size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
						if err != nil || size < 0 {
							t.Errorf("invalid RESP bulk: %q", line)
							return
						}
						value := make([]byte, size+2)
						if _, err := io.ReadFull(r, value); err != nil {
							return
						}
						args[i] = string(value[:size])
					}
					name := strings.ToLower(args[0])
					mu.Lock()
					commands = append(commands, name)
					mu.Unlock()
					if name == fail && loseReply {
						return
					}
					reply := ":1\r\n"
					if name == fail {
						reply = "-WRONGTYPE injected rejection\r\n"
					}
					if _, err := io.WriteString(server, reply); err != nil {
						return
					}
				}
			}()
			return client, nil
		}})
	t.Cleanup(func() {
		_ = rds.Close()
		mu.Lock()
		for _, peer := range peers {
			_ = peer.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return rds, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), commands...) }
}

func TestLegacyMessageAcceptance(t *testing.T) {
	// The production logger is lazy; keep its first initialization in a temp dir.
	oldPath := config.Conf.LOG.Path
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	logPath, err := filepath.Rel(config.FindProjectRoot(wd), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config.Conf.LOG.Path = logPath
	logger := utils.Logger()
	config.Conf.LOG.Path = oldPath
	if file, ok := logger.Out.(*os.File); ok {
		t.Cleanup(func() { _ = file.Close(); logger.SetOutput(io.Discard) })
	}
	for _, tc := range []struct {
		name, fail           string
		loseReply, wantError bool
		commands             []string
	}{
		{"success", "", false, false, []string{"rpush", "zadd", "publish"}},
		{"enqueue_rejected", "rpush", false, true, []string{"rpush"}},
		{"enqueue_reply_lost", "rpush", true, true, []string{"rpush"}},
		{"cache_rejected", "zadd", false, false, []string{"rpush", "zadd", "publish"}},
		{"notify_rejected", "publish", false, false, []string{"rpush", "zadd", "publish"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rds, commands := acceptanceRedis(t, tc.fail, tc.loseReply)
			msg := &models.Message{Id: "acceptance", SenderId: 1, ReceiverId: 2, Type: models.PrivateType}
			err := NewMessageManager(nil, rds).PublishAndSave(context.Background(), msg)
			if (err != nil) != tc.wantError {
				t.Errorf("error=%v, wantError=%v", err, tc.wantError)
			}
			if tc.wantError && (!errors.Is(err, ErrMessageEnqueueUnknown) || !errors.Is(err, ErrStorageUnavailable)) {
				t.Errorf("enqueue classification: %v", err)
			}
			// net.Pipe can report EOF or ErrClosedPipe depending on whether
			// the peer closes before the client sets its read deadline.
			if tc.loseReply && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("lost original cause: %v", err)
			}
			if got := commands(); !reflect.DeepEqual(got, tc.commands) {
				t.Errorf("commands=%v, want %v", got, tc.commands)
			}
		})
	}
}

func TestLegacyMessagePreCanceled(t *testing.T) {
	rds, commands := acceptanceRedis(t, "", false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NewMessageManager(nil, rds).SaveMsg(ctx, &models.Message{SenderId: 1, ReceiverId: 2, Type: models.PrivateType})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStorageUnavailable) || errors.Is(err, ErrMessageEnqueueUnknown) {
		t.Fatalf("error=%v", err)
	}
	if got := commands(); len(got) != 0 {
		t.Fatalf("canceled request wrote %v", got)
	}
}
