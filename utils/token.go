package utils

import (
	"context"
	"errors"
	"fmt"
	"iChat/config"
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

// 校验jwt是否有效 返回claims
func TokenValid(c *gin.Context) (jwt.MapClaims, error) {
	tokenString, err := ExtractToken(c)
	if err != nil {
		return nil, err
	}
	fmt.Println("token: ", tokenString)
	// 检查token是否在黑名单中
	if err = RDS.Get(context.Background(), tokenString).Err(); err == nil {
		// 在黑名单中
		return nil, errors.New("token banned")
	}
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(config.Conf.JWT.Key), nil
	}, jwt.WithExpirationRequired())

	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if ok && token.Valid {
		return claims, nil
	}
	return nil, errors.New("token invalid")
}

// 从请求头或query中获取token
func ExtractToken(c *gin.Context) (string, error) {
	// 格式为bearer [token] 或者 [token]
	if bearerToken := c.GetHeader("Authorization"); bearerToken != "" {
		if len(strings.Split(bearerToken, " ")) == 2 {
			return strings.Split(bearerToken, " ")[1], nil
		} else {
			return bearerToken, nil
		}
	}
	//token在query中
	if t := c.Query("token"); t != "" {
		return t, nil
	}
	if t := c.Request.FormValue("token"); t != "" {
		return t, nil
	}
	return "", errors.New("token not found")
}

// 封禁token 将其加入redis黑名单
func BanToken(token string, claims jwt.MapClaims) error {
	expTime, err := claims.GetExpirationTime()
	if err != nil {
		return err
	}
	// 剩余时间为过期时间减去当前时间
	leftTime := expTime.Unix() - time.Now().Unix()
	// 加入redis黑名单
	RDS.Set(context.Background(), token, 0, time.Duration(leftTime)*time.Second)
	return nil
}
