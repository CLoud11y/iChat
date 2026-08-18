package config

import (
	"os"
	"path/filepath"
	"strings"

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
	configureConfigFile()
	viper.SetEnvPrefix("ICHAT")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		panic("Error reading config file (copy config/config.example.yml to config/config.yml): " + err.Error())
	}

	if err := viper.Unmarshal(&Conf); err != nil {
		panic("Unable to decode into struct: " + err.Error())
	}
}

func configureConfigFile() {
	if configFile := os.Getenv("ICHAT_CONFIG_FILE"); configFile != "" {
		viper.SetConfigFile(configFile)
		return
	}

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./config")
	viper.AddConfigPath(".")

	if workingDirectory, err := os.Getwd(); err == nil {
		if projectRoot := FindProjectRoot(workingDirectory); projectRoot != "" {
			viper.AddConfigPath(filepath.Join(projectRoot, "config"))
		}
	}

	if executable, err := os.Executable(); err == nil {
		viper.AddConfigPath(filepath.Join(filepath.Dir(executable), "config"))
	}
}

func FindProjectRoot(start string) string {
	directory, err := filepath.Abs(start)
	if err != nil {
		return ""
	}

	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}
