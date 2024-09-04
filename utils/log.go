package utils

import (
	"iChat/config"
	"log"
	"os"
)

var Logger *log.Logger

func init() {
	logFile, err := os.OpenFile(config.Conf.LOG.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}
	Logger = log.New(logFile, "[ichat]", log.Llongfile|log.Ldate|log.Ltime)
}
