package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	fiber "github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/proxy"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/valyala/fasthttp"
)

type Config struct {
	Backend string `json:"backend"`
}

func loadOrCreateConfig(path string) (Config, error) {
	var cfg Config
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		decoder := json.NewDecoder(file)
		if err := decoder.Decode(&cfg); err == nil && cfg.Backend != "" {
			return cfg, nil
		}
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Write backend (example: http://127.0.0.1:80): ")
	backend, _ := reader.ReadString('\n')
	backend = strings.TrimSpace(backend)
	cfg.Backend = backend
	f, err := os.Create(path)
	if err == nil {
		encoder := json.NewEncoder(f)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(cfg)
		f.Close()
	}
	return cfg, nil
}

func main() {
	cfg, _ := loadOrCreateConfig("config.json")

	client := &fasthttp.Client{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	app := fiber.New(fiber.Config{
		Prefork: true,
	})

	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(compress.New(compress.Config{
		Level: compress.LevelBestSpeed,
	}))

	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,HEAD,PUT,DELETE,PATCH",
		AllowHeaders: "Origin, Content-Type, Accept",
	}))

	app.Use(func(c *fiber.Ctx) error {
		return proxy.Do(c, cfg.Backend+c.OriginalURL(), client)
	})

	fmt.Printf("[%s] Started reverse proxy: %s\n", time.Now().Format("2006-01-02 15:04:05"), cfg.Backend)
	app.Listen(":80")
}
