// Command mqadmin manages local MQ topology and queries quarantine audits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/spf13/viper"
	"iChat/messaging/rabbitmq"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) (err error) {
	if len(args) > 0 && args[0] == "quarantine" {
		return runQuarantine(args[1:], output, openQuarantineReader)
	}
	if len(args) == 0 || (args[0] != "init" && args[0] != "check") {
		return errors.New("usage: mqadmin init|check [--env-file .env] | quarantine list|show [options]")
	}
	flags := flag.NewFlagSet("mqadmin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	envFile := flags.String("env-file", ".env", "explicit local Compose environment file; empty uses process environment only")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return errors.New("usage: mqadmin init|check [--env-file .env] | quarantine list|show [options]")
	}
	settings, err := loadSettings(*envFile)
	if err != nil {
		return err
	}
	if args[0] == "init" && (settings.appPassword == "" || settings.appPassword == settings.adminPassword) {
		return errors.New("RABBITMQ_APP_PASSWORD must be nonempty and different from RABBITMQ_ADMIN_PASSWORD")
	}
	m, err := rabbitmq.NewManagement("http://127.0.0.1:"+settings.managementPort, "ichat_admin", settings.adminPassword)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if args[0] == "check" {
		if err := rabbitmq.VerifyLocal(ctx, m, rabbitmq.LocalVHost); err != nil {
			return err
		}
		fmt.Fprintln(output, "local RabbitMQ topology verified (single node)")
		return nil
	}
	// Inspect before account creation as well as before AMQP declarations.
	if err := rabbitmq.PreflightLocal(ctx, m, rabbitmq.LocalVHost); err != nil {
		return err
	}
	if err := m.ProvisionLocalUser(ctx, settings.appPassword); err != nil {
		return err
	}
	u := &url.URL{Scheme: "amqp", Host: "127.0.0.1:" + settings.amqpPort, User: url.UserPassword(rabbitmq.LocalAppUser, settings.appPassword), Path: "/" + rabbitmq.LocalVHost}
	// Bound this one-shot administration connection, including its AMQP RPCs.
	// The service publisher/supervisor transport is implemented separately.
	deadline, _ := ctx.Deadline()
	var socket net.Conn
	conn, err := amqp.DialConfig(u.String(), amqp.Config{Recovery: nil, Heartbeat: 2 * time.Second, Dial: func(network, address string) (net.Conn, error) {
		socket, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := socket.SetDeadline(deadline); err != nil {
			socket.Close()
			return nil, err
		}
		return socket, nil
	}})
	if err != nil {
		if socket != nil {
			socket.Close()
		}
		return errors.New("RabbitMQ application AMQP authentication or connection failed")
	}
	defer func() {
		closeErr := conn.CloseDeadline(time.Now().Add(3 * time.Second))
		socket.Close()
		if closeErr != nil && !errors.Is(closeErr, amqp.ErrClosed) {
			err = errors.Join(err, errors.New("close RabbitMQ administration connection failed"))
		}
	}()
	// The client clears handshake deadlines on successful open.
	if err := socket.SetDeadline(deadline); err != nil {
		return errors.New("set RabbitMQ administration deadline failed")
	}
	ch, err := conn.Channel()
	if err != nil {
		return errors.New("open RabbitMQ topology channel failed")
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, rabbitmq.LocalVHost); err != nil {
		return err
	}
	fmt.Fprintln(output, "local RabbitMQ topology initialized and verified (single node)")
	return nil
}

type settings struct{ adminPassword, appPassword, amqpPort, managementPort string }

func loadSettings(envFile string) (settings, error) {
	v := viper.New()
	if envFile != "" {
		v.SetConfigFile(envFile)
		v.SetConfigType("env")
		if err := v.ReadInConfig(); err != nil {
			return settings{}, errors.New("read RabbitMQ environment file failed")
		}
	}
	v.SetDefault("RABBITMQ_PORT", "5672")
	v.SetDefault("RABBITMQ_MANAGEMENT_PORT", "15672")
	for _, key := range []string{"RABBITMQ_ADMIN_PASSWORD", "RABBITMQ_APP_PASSWORD", "RABBITMQ_PORT", "RABBITMQ_MANAGEMENT_PORT"} {
		if value, ok := os.LookupEnv(key); ok {
			v.Set(key, value)
		}
	}
	s := settings{v.GetString("RABBITMQ_ADMIN_PASSWORD"), v.GetString("RABBITMQ_APP_PASSWORD"), v.GetString("RABBITMQ_PORT"), v.GetString("RABBITMQ_MANAGEMENT_PORT")}
	if s.adminPassword == "" {
		return settings{}, errors.New("RABBITMQ_ADMIN_PASSWORD is required")
	}
	for _, port := range []string{s.amqpPort, s.managementPort} {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return settings{}, errors.New("RabbitMQ ports must be integers between 1 and 65535")
		}
	}
	// check is read-only and does not require an application password.
	return s, nil
}
