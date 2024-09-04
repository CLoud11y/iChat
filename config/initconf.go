package config

import (
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
	JWT struct {
		TokenHourLifeSpan int    `mapstructure:"tokenHourLifeSpan"`
		Key               string `mapstructure:"key"`
	} `mapstructure:"JWT"`
	REDIS struct {
		Addr        string `mapstructure:"addr"`
		Password    string `mapstructure:"password"`
		DB          int    `mapstructure:"db"`
		PoolSize    int    `mapstructure:"poolSize"`
		MinIdleConn int    `mapstructure:"minIdleConn"`
	} `mapstructure:"REDIS"`
	LOG struct {
		Path string `mapstructure:"path"`
	} `mapstructure:"LOG"`
}

func init() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("c:\\Users\\96316\\vscodeProjects\\goProjects\\iChat\\config")

	if err := viper.ReadInConfig(); err != nil {
		panic("Error reading config file: " + err.Error())
	}

	if err := viper.Unmarshal(&Conf); err != nil {
		panic("Unable to decode into struct: " + err.Error())
	}
	// fmt.Println(Conf.App)
	// fmt.Println(Conf.MYSQL)
}
