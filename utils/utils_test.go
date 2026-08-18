package utils

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/mozillazg/go-pinyin"
)

func Test_s2b(t *testing.T) {
	s := "hello world hello world hello world hello world"
	bs := String2Bytes(s)
	if !bytes.Equal(bs, []byte(s)) {
		t.Fatal("String2Bytes error")
	}
}

func Test_b2s(t *testing.T) {
	bs := []byte("hello world hello world hello world hello world")
	s := Bytes2String(bs)
	if s != string(bs) {
		t.Fatal("Bytes2String error")
	}
}

func TestVerifyPassword(t *testing.T) {
	encrypted := Encrypt("correct-password")
	if !VerifyPassword("correct-password", encrypted) {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword("wrong-password", encrypted) {
		t.Fatal("VerifyPassword accepted an incorrect password")
	}
	if VerifyPassword("correct-password", "invalid-hash") {
		t.Fatal("VerifyPassword accepted an invalid hash")
	}
}

func Benchmark_s2b(b *testing.B) {
	s := "hello world hello world hello world hello world"
	for i := 0; i < b.N; i++ {
		_ = String2Bytes(s)
	}
}

func Benchmark_b2s(b *testing.B) {
	bs := []byte("hello world hello world hello world hello world")
	for i := 0; i < b.N; i++ {
		_ = Bytes2String(bs)
	}
}

func TestPinYin(t *testing.T) {
	strs := []string{"你好，世界", "yuyuyuyu", "<eiq", "6219"}
	for _, str := range strs {
		if str[0] >= 'a' && str[0] <= 'z' {
			fmt.Println(string(str[0]))
		} else {
			t := pinyin.Convert(str, nil)
			if len(t) > 0 {
				fmt.Println(string(t[0][0][0]))
			} else {
				fmt.Println("#")
			}
		}
	}
}
