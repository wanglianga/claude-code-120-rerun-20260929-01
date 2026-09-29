package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	Port      string
	PGDSN     string
	RedisAddr string
	RedisPwd  string
	RedisDB   int
}

func loadConfig() Config {
	c := Config{Port: env("PORT", "8080"), RedisAddr: env("REDIS_ADDR", "localhost:6379"), RedisPwd: os.Getenv("REDIS_PASSWORD"), RedisDB: envInt("REDIS_DB", 0)}
	// 支持 DATABASE_URL 或 PG* 离散变量
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		c.PGDSN = dsn
	} else {
		sslmode := env("PGSSLMODE", "disable")
		c.PGDSN = fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
			env("PGHOST", "localhost"), env("PGPORT", "5433"), env("PGUSER", "seafood"),
			os.Getenv("PGPASSWORD"), env("PGDATABASE", "seafood"), sslmode)
	}
	return c
}

func main() {
	cfg := loadConfig()
	app, err := NewApp(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
	defer app.close()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		fmt.Printf("海鲜预制菜批次解冻加工与召回服务监听 :%s\n", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "HTTP 服务异常: %v\n", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	fmt.Println("服务已停止")
}

func newRedisClient(cfg Config) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPwd, DB: cfg.RedisDB})
}
