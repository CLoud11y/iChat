package utils

import (
	"errors"
	"fmt"
	"iChat/config"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func GenerateToken(user_id uint) (string, error) {
	claims := jwt.MapClaims{
		"authorized": true,
		"user_id":    user_id,
		"exp":        time.Now().Add(time.Hour * time.Duration(config.Conf.JWT.TokenHourLifeSpan)).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.Conf.JWT.Key))
}

func TokenValid(c *gin.Context) (uint, error) {
	tokenString := ExtractToken(c)
	// fmt.Println("token: ", tokenString)
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(config.Conf.JWT.Key), nil
	}, jwt.WithExpirationRequired())

	if err != nil {
		return 0, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if ok && token.Valid {
		// 如果jwt有效，将user_id转换为浮点数字符串，然后再转换为 uint32
		uid, err := strconv.ParseUint(fmt.Sprintf("%.0f", claims["user_id"]), 10, 32)
		if err != nil {
			return 0, err
		}
		return uint(uid), nil
	}
	return 0, errors.New("token invalid")
}

// 从请求头或query中获取token
func ExtractToken(c *gin.Context) string {
	bearerToken := c.GetHeader("Authorization")
	// 格式为bearer [token]
	if len(strings.Split(bearerToken, " ")) == 2 {
		return strings.Split(bearerToken, " ")[1]
	} else if t := c.Query("token"); t != "" {
		return t
	}
	return ""
}
