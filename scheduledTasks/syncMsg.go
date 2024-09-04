package scheduledtasks

import (
	"iChat/database"
	"iChat/utils"
)

func SyncMsg() {
	msgs, size, err := database.Mmanager.GetAllDirtyMsgs()
	if err != nil {
		utils.Logger.Panicln(err)
	}
	if size == 0 {
		return
	}
	err = database.Mmanager.SaveMsgs2DB(msgs)
	if err != nil {
		utils.Logger.Panicln(err)
	}
	err = database.Mmanager.RemDirtyMsgs(size)
	if err != nil {
		utils.Logger.Panicln(err)
	}
}
