package utils

import (
	"iChat/config"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	applicationLogger *logrus.Logger
	loggerOnce        sync.Once
)

// Logger returns the process-wide logger. Keeping one logger avoids opening a
// new file descriptor for every log entry.
func Logger() *logrus.Logger {
	loggerOnce.Do(initLogger)
	return applicationLogger
}

func initLogger() {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})
	applicationLogger = logger

	workingDirectory, err := os.Getwd()
	if err != nil {
		logger.WithError(err).Error("get working directory for log file failed")
		return
	}
	if projectRoot := config.FindProjectRoot(workingDirectory); projectRoot != "" {
		workingDirectory = projectRoot
	}
	logDirectory := filepath.Join(workingDirectory, strings.TrimLeft(config.Conf.LOG.Path, `/\`))
	if err := os.MkdirAll(logDirectory, 0755); err != nil {
		logger.WithError(err).Error("create log directory failed")
		return
	}
	fileName := filepath.Join(logDirectory, time.Now().Format("2006-01-02")+".log")
	file, err := os.OpenFile(fileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		logger.WithError(err).Error("open log file failed")
		return
	}
	logger.Out = file
}
