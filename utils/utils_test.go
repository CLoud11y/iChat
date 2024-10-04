package utils

import (
	"bytes"
	"testing"
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
