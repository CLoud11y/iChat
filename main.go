package main

import (
	"fmt"
	"iChat/config"
	"iChat/router"
)

func main() {
	r := router.Router()
	r.Run() // listen and serve on 0.0.0.0:8080 (for windows "localhost:8080")
	fmt.Println(config.Conf.App)
}
