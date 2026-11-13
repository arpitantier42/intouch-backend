// Command api serves the InTouch HTTP API.
package main

import (
	"log"
	"os"

	"github.com/intouch/api/internal/server"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if err := server.New().Run(":" + port); err != nil {
		log.Fatal(err)
	}
}
