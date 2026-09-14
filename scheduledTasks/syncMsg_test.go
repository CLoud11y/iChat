//go:build integration

package scheduledtasks

import (
	"testing"
)

func TestSyncMsg(t *testing.T) {
	t.Skip("P0: legacy sync integration test disabled until isolated fixtures and state assertions exist")
	t.Log("TestSyncMsg start")

	SyncMsg()

	t.Log("TestSyncMsg end")
}
