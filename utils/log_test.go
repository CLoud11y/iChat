package utils

import (
	"iChat/config"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestLogger(t *testing.T) {
	// Keep filesystem output within this test's temporary directory.
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldConf := config.Conf
	logDirectory := t.TempDir()
	if err := os.Chdir(logDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		config.Conf = oldConf
		if err := os.Chdir(workingDirectory); err != nil {
			t.Error(err)
		}
	})
	config.Conf.LOG.Path = "logs"
	config.Conf.LOG.Level = "info"
	log := Logger()
	file, ok := log.Out.(*os.File)
	if !ok {
		t.Fatal("logger did not open its output file")
	}
	t.Cleanup(func() {
		_ = file.Close()
		applicationLogger = nil
		loggerOnce = sync.Once{}
	})
	if log != Logger() {
		t.Fatal("Logger must reuse the same instance")
	}
	log.WithFields(logrus.Fields{"name": "test", "age": 18}).Info("test log")
	if _, err := os.Stat(filepath.Join(logDirectory, "logs", time.Now().Format("2006-01-02")+".log")); err != nil {
		t.Fatal(err)
	}
}
