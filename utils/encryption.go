package utils

import (
	"crypto/md5"
	"encoding/hex"
)

func Encrypt(str string) string {
	h := md5.New()
	h.Write([]byte(str))
	md5_str := hex.EncodeToString(h.Sum(nil))
	return md5_str
}
