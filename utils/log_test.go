package utils

import (
	"testing"

	"github.com/sirupsen/logrus"
)

func TestLogger(t *testing.T) {
	log := Logger()
	log.WithFields(logrus.Fields{"name": "test", "age": 18}).Info("test log")
}
