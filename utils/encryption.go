package utils

import (
	"crypto/md5"
	"encoding/hex"
	"unsafe"
)

func Encrypt(str string) string {
	h := md5.New()
	h.Write([]byte(str))
	md5_str := hex.EncodeToString(h.Sum(nil))
	return md5_str
}

func String2Bytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func Bytes2String(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}
