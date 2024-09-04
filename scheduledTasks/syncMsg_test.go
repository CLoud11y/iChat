package scheduledtasks

import (
	"testing"
)

func TestSyncMsg(t *testing.T) {
	t.Log("TestSyncMsg start")

	SyncMsg()

	t.Log("TestSyncMsg end")
}
