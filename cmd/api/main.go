// Command api serves the InTouch HTTP API.
package main

import (
	"context"
	"log"
	"os"

	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/server"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal(err)
		}
		log.Printf("warning: %v (authenticated routes answer 401 until it is set)", err)
	}
	log.Printf("config: %s", cfg)

	deps := server.Deps{Config: cfg}
	if cfg.DatabaseURL != "" {
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("database: %v", err)
		}
		defer d.Close()
		deps.DB = d
	}
	if err := server.New(deps).Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
