package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type RespInfo struct {
	Code  int         `json:"Code"` //0成功 -1失败
	Msg   string      `json:"Msg"`
	Data  interface{} `json:"Data"`
	Rows  interface{} `json:"Rows"`
	Total interface{} `json:"Total"`
}

func resp(w http.ResponseWriter, code int, data interface{}, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	h := RespInfo{
		Code: code,
		Data: data,
		Msg:  msg,
	}
	ret, err := json.Marshal(h)
	if err != nil {
		fmt.Println(err)
	}
	w.Write(ret)
}

func respList(w http.ResponseWriter, code int, data interface{}, total interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	h := RespInfo{
		Code:  code,
		Rows:  data,
		Total: total,
	}
	ret, err := json.Marshal(h)
	if err != nil {
		fmt.Println(err)
	}
	w.Write(ret)
}

func RespFail(w http.ResponseWriter, msg string) {
	resp(w, -1, nil, msg)
}

func RespOK(w http.ResponseWriter, data interface{}, msg string) {
	resp(w, 0, data, msg)
}

func RespOKList(w http.ResponseWriter, data interface{}, total interface{}) {
	respList(w, 0, data, total)
}
