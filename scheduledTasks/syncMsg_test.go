//go:build integration

package scheduledtasks

import (
	"context"
	"iChat/config"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSyncMsg(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	previous := database.Mmanager
	database.Mmanager = database.NewMessageManager(f.DB, f.Redis)
	t.Cleanup(func() { database.Mmanager = previous })
	isolateSyncLogger(t)
	messages := []models.Message{
		f.Message(f.Users[0].ID, f.Users[1].ID, models.PrivateType, models.TextMedia, "hello"),
		f.Message(f.Users[1].ID, f.Users[0].ID, models.PrivateType, models.PictureMedia, "/fixture/image.png"),
		f.Message(f.Users[0].ID, f.Groups[0].ID, models.GroupType, models.AudioMedia, "/fixture/audio.ogg"),
	}
	for i := range messages {
		if err := database.Mmanager.SaveMsg(&messages[i]); err != nil {
			t.Fatal(err)
		}
	}
	assertSyncState(t, f, nil, int64(len(messages)))
	SyncMsg()
	assertSyncState(t, f, messages, 0)
	// A second invocation with an empty queue must not change committed data.
	SyncMsg()
	assertSyncState(t, f, messages, 0)
}

func assertSyncState(t *testing.T, f *testenv.LegacyFixture, want []models.Message, queued int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []models.Message
	if err := f.DB.WithContext(ctx).Order("id").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("persisted count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("persisted message = %#v, want %#v", got[i], want[i])
		}
	}
	if size, err := f.Redis.LLen(ctx, "dirty:msgs").Result(); err != nil || size != queued {
		t.Fatalf("dirty queue length = %d, want %d; err=%v", size, queued, err)
	}
}

// The legacy worker calls the process-wide logger. Keep its file in this
// test's temporary directory, then leave the singleton writing to io.Discard
// after closing the file, so repeated runs never use a closed file handle.
func isolateSyncLogger(t *testing.T) {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	logPath, err := filepath.Rel(config.FindProjectRoot(workingDirectory), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previous := config.Conf
	config.Conf.LOG.Path, config.Conf.LOG.Level = logPath, "info"
	logger := utils.Logger()
	config.Conf = previous
	output := logger.Out
	t.Cleanup(func() {
		logger.SetOutput(io.Discard)
		if file, ok := output.(*os.File); ok && file != os.Stdout && file != os.Stderr {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	})
}
