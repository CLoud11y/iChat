package utils

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"unsafe"

	"github.com/mozillazg/go-pinyin"
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

func GetFirstLetter(str string) string {
	if len(str) == 0 {
		return ""
	}
	firstLetter := "#"
	if (str[0] >= 'a' && str[0] <= 'z') || (str[0] >= 'A' && str[0] <= 'Z') {
		firstLetter = string(str[0])
	} else {
		t := pinyin.Convert(str, nil)
		if len(t) > 0 {
			firstLetter = string(t[0][0][0])
		}
	}
	return strings.ToUpper(firstLetter)
}
