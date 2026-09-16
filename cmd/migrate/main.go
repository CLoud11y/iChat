// Command migrate explicitly applies the current legacy MySQL schema.
package main

import (
	"errors"
	"fmt"
	"iChat/config"
	"iChat/utils"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "legacy schema migration completed")
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
	if err := utils.MigrateLegacy(db); err != nil {
		return fmt.Errorf("migrate legacy schema: %w", err)
	}
	return nil
}
