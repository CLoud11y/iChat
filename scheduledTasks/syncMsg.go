package scheduledtasks

import (
	"iChat/database"
	"iChat/utils"
)

func SyncMsg() {
	defer func() {
		if r := recover(); r != nil {
			utils.Logger().WithField("panic", r).Error("sync messages failed")
		}
	}()
	msgs, size, err := database.Mmanager.GetAllDirtyMsgs()
	if err != nil {
		panic(err)
	}
	if size == 0 {
		return
	}
	err = database.Mmanager.SaveMsgs2DB(msgs)
	if err != nil {
		panic(err)
	}
	// todo: 清理失败怎么办？可能造成多次插入mysql 主键冲突插入失败
	err = database.Mmanager.RemDirtyMsgs(size)
	if err != nil {
		panic(err)
	}
	utils.Logger().WithField("size", size).Info("sync messages completed")
}
