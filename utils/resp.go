package utils

import (
	"encoding/json"
	"net/http"
)

type RespInfo struct {
	Code  int         `json:"code"` //0成功 -1失败
	Msg   string      `json:"msg"`
	Data  interface{} `json:"data"`
	Rows  interface{} `json:"rows"`
	Total interface{} `json:"total"`
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
		Logger().WithError(err).Error("marshal response failed")
		return
	}
	if _, err := w.Write(ret); err != nil {
		Logger().WithError(err).Warn("write response failed")
	}
}

func respList(w http.ResponseWriter, code int, data interface{}, total interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	h := RespInfo{
		Code:  code,
		Data:  data,
		Total: total,
	}
	ret, err := json.Marshal(h)
	if err != nil {
		Logger().WithError(err).Error("marshal list response failed")
		return
	}
	if _, err := w.Write(ret); err != nil {
		Logger().WithError(err).Warn("write list response failed")
	}
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
