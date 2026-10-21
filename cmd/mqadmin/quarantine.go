package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"iChat/database"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"
)

const quarantineUsage = "usage: mqadmin quarantine list [--state open|replay_pending|resolved|all] [--after transport-id] [--limit 1..100] [--config path] | show --transport-id id [--include-raw] [--config path]"

type quarantineReader interface {
	List(context.Context, database.QuarantineListOptions) (database.QuarantinePage, error)
	Get(context.Context, string) (database.QuarantineDetail, error)
}

type quarantineOpen func(string) (quarantineReader, func() error, error)

// Parse and validate selection before loading credentials or opening SQL.
func runQuarantine(args []string, output io.Writer, open quarantineOpen) (err error) {
	if len(args) == 0 || (args[0] != "list" && args[0] != "show") {
		return errors.New(quarantineUsage)
	}
	flags := flag.NewFlagSet("quarantine", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configFile := os.Getenv("ICHAT_CONFIG_FILE")
	if configFile == "" {
		configFile = "config/config.yml"
	}
	file := flags.String("config", configFile, "SQL configuration file; empty uses process environment only")
	var options database.QuarantineListOptions
	var id string
	var raw bool
	if args[0] == "list" {
		flags.StringVar(&options.State, "state", "open", "audit state or all")
		flags.StringVar(&options.After, "after", "", "exclusive transport ID cursor")
		flags.IntVar(&options.Limit, "limit", 50, "page size between 1 and 100")
	} else {
		flags.StringVar(&id, "transport-id", "", "exact audit transport ID")
		flags.BoolVar(&raw, "include-raw", false, "include base64 source and complete typed audit JSON")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return errors.New(quarantineUsage)
	}
	if (args[0] == "list" && options.Validate() != nil) || (args[0] == "show" && !database.ValidQuarantineTransportID(id)) {
		return errors.New(quarantineUsage)
	}
	reader, closeReader, err := open(*file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeReader()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var result any
	if args[0] == "list" {
		page, err := reader.List(ctx, options)
		if err != nil {
			return err
		}
		result = struct {
			database.QuarantinePage
			AuditVerified bool `json:"audit_verified"`
		}{QuarantinePage: page}
	} else {
		detail, err := reader.Get(ctx, id)
		if err != nil {
			return err
		}
		view := struct {
			database.QuarantineSummary
			AuditVerified bool            `json:"audit_verified"`
			Format        string          `json:"format"`
			RawBody       []byte          `json:"raw_body_base64,omitempty"`
			Audit         json.RawMessage `json:"audit,omitempty"`
		}{QuarantineSummary: detail.Summary, AuditVerified: true, Format: string(detail.Input.Format())}
		if raw {
			source, err := detail.Input.Envelope().Source()
			if err != nil {
				return err
			}
			view.RawBody = source.Body
			view.Audit, err = detail.Input.EncodeAudit()
			if err != nil {
				return err
			}
		}
		result = view
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return errors.New("write quarantine query output failed")
	}
	return nil
}

// Only MYSQL keys are loaded. This command never reads Compose .env, validates
// runtime roles, connects Redis/MQ, migrates tables or opens a business service.
func openQuarantineReader(file string) (quarantineReader, func() error, error) {
	options, err := loadQuarantineMySQL(file)
	if err != nil {
		return nil, nil, err
	}
	connector, err := mysql.NewConnector(options)
	if err != nil {
		return nil, nil, errors.New("configure quarantine SQL connection failed")
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	reader, err := database.NewQuarantineRepository(pool)
	if err != nil {
		_ = pool.Close()
		return nil, nil, err
	}
	return reader, func() error {
		if pool.Close() != nil {
			return errors.New("close quarantine SQL connection failed")
		}
		return nil
	}, nil
}

func loadQuarantineMySQL(file string) (*mysql.Config, error) {
	v := viper.New()
	if file != "" {
		v.SetConfigFile(file)
		if v.ReadInConfig() != nil {
			return nil, errors.New("read quarantine SQL configuration failed")
		}
	}
	for _, key := range []string{"user", "password", "host", "port", "dbname"} {
		if value, ok := os.LookupEnv("ICHAT_MYSQL_" + strings.ToUpper(key)); ok {
			v.Set("MYSQL."+key, value)
		}
	}
	port, err := strconv.Atoi(v.GetString("MYSQL.port"))
	if err != nil || port < 1 || port > 65535 || v.GetString("MYSQL.user") == "" || v.GetString("MYSQL.host") == "" || v.GetString("MYSQL.dbname") == "" {
		return nil, errors.New("quarantine MYSQL user, host, valid port and dbname are required")
	}
	options := mysql.NewConfig()
	options.User, options.Passwd, options.DBName = v.GetString("MYSQL.user"), v.GetString("MYSQL.password"), v.GetString("MYSQL.dbname")
	options.Net, options.Addr = "tcp", net.JoinHostPort(v.GetString("MYSQL.host"), strconv.Itoa(port))
	options.ParseTime, options.Loc = true, time.UTC
	options.Timeout, options.ReadTimeout, options.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	options.Params = map[string]string{"charset": "utf8mb4"}
	return options, nil
}
