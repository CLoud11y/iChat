// Command migrate-reliable applies the v1 reliability schema to a fresh MySQL database.
package main

import (
	"context"
	"errors"
	"fmt"
	"iChat/config"
	"iChat/migrations"
	"iChat/utils"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "reliable schema migration v1 completed")
}

func run() (err error) {
	conf, err := config.Load()
	if err != nil {
		return err
	}
	db, err := utils.OpenMySQL(conf)
	if err != nil {
		return err
	}
	pool, err := db.DB()
	if err != nil {
		return fmt.Errorf("get mysql connection pool: %w", err)
	}
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close mysql connection pool: %w", closeErr))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		return fmt.Errorf("migrate reliable schema: %w", err)
	}
	return nil
}
