package config

import (
	"fmt"

	"github.com/spf13/viper"
)

var Conf Config

type Config struct {
	App struct {
		Name    string `mapstructure:"name"`
		Version string `mapstructure:"version"`
	} `mapstructure:"app"`
	MYSQL struct {
		User     string `mapstructure:"user"`
		Password string `mapstructure:"password"`
		Host     string `mapstructure:"host"`
		Port     string `mapstructure:"port"`
		DbName   string `mapstructure:"dbname"`
	} `mapstructure:"MYSQL"`
}

func init() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./config")

	if err := viper.ReadInConfig(); err != nil {
		fmt.Printf("Error reading config file, %s", err)
	}

	if err := viper.Unmarshal(&Conf); err != nil {
		fmt.Printf("Unable to decode into struct, %v", err)
	}
	// fmt.Println(Conf.App)
	// fmt.Println(Conf.MYSQL.User)
}
