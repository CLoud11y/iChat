//go:build integration

package testenv

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/messaging/rabbitmq"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
)

type quarantineAuditChildConfig struct{ DSN, AMQPURL, ManagementURL, VHost, Password, Stage string }

func quarantineAuditCrashWorker(t *testing.T, f *persistenceLifecycleFixture, stage string) func() {
	t.Helper()
	conf := quarantineAuditChildConfig{DSN: f.dsn.FormatDSN(), AMQPURL: f.broker.AMQPURL(), ManagementURL: "http://" + f.broker.ManagementAddress, VHost: f.broker.VHost, Password: f.env.password, Stage: stage}
	raw, err := json.Marshal(conf)
	if err != nil {
		t.Fatal("child configuration encoding failed")
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(program, "-test.run=^TestQuarantineAuditCrashHelper$", "-test.timeout=60s")
	// Pass fixture-only credentials through the private child environment;
	// never put them in command arguments, diagnostics or development config.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "ICHAT_QUARANTINE_AUDIT_CHILD=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ICHAT_QUARANTINE_AUDIT_CHILD="+string(raw))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done, barrier := make(chan struct{}), make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "quarantine-audit-commit-barrier" {
				close(barrier)
				return
			}
		}
	}()
	var once sync.Once
	kill := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			select {
			case <-done:
				if waitErr == nil {
					t.Error("child completed instead of being killed")
				}
			case <-time.After(5 * time.Second):
				t.Error("audit child did not terminate")
			}
		})
	}
	t.Cleanup(kill)
	select {
	case <-barrier:
	case <-done:
		t.Fatal("audit child exited before SQL barrier", waitErr)
	case <-time.After(25 * time.Second):
		t.Fatal("audit child did not reach SQL barrier")
	}
	return kill
}

func TestQuarantineAuditCrashHelper(t *testing.T) {
	raw := os.Getenv("ICHAT_QUARANTINE_AUDIT_CHILD")
	if raw == "" {
		t.Skip("private subprocess helper")
	}
	var conf quarantineAuditChildConfig
	if json.Unmarshal([]byte(raw), &conf) != nil || (conf.Stage != "before_commit" && conf.Stage != "after_commit") {
		t.Fatal("invalid child configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	settings, err := mysql.ParseDSN(conf.DSN)
	if err != nil {
		t.Fatal("child SQL configuration failed")
	}
	settings.InterpolateParams = true
	base, err := mysql.NewConnector(settings)
	if err != nil {
		t.Fatal("child connector failed")
	}
	entered, release := make(chan context.Context, 1), make(chan struct{})
	fault := &quarantineAuditFaultConnector{base: base, stage: conf.Stage, entered: entered, release: release}
	fault.armed.Store(true)
	pool := sql.OpenDB(fault)
	defer pool.Close()
	management, err := rabbitmq.NewManagement(conf.ManagementURL, rabbitAdminUser, conf.Password)
	if err != nil {
		t.Fatal("child management failed")
	}
	session, err := rabbitmq.DialConsumer(ctx, conf.AMQPURL, management, conf.VHost, rabbitmq.DefaultConnectionOptions())
	if err != nil {
		t.Fatal("child MQ connection failed")
	}
	defer session.Close(context.Background())
	o := workers.DefaultQuarantineOptions()
	o.ProcessingTimeout = 40 * time.Second
	service, err := workers.NewQuarantineService(pool, []workers.QuarantineConsumer{{Session: session, Prefetch: 1}}, o)
	if err != nil {
		t.Fatal("child service failed")
	}
	if err := service.Start(ctx, ctx); err != nil {
		t.Fatal("child activation failed")
	}
	select {
	case <-entered:
		fmt.Println("quarantine-audit-commit-barrier")
	case <-service.Done():
		t.Fatal("child stopped before SQL barrier")
	case <-ctx.Done():
		t.Fatal("child SQL barrier expired")
	}
	// The parent kills this exact process while a real audit transaction is
	// held before COMMIT or after COMMIT and before the handler's ACK.
	<-ctx.Done()
	close(release)
	_ = service.Close(context.Background())
	t.Fatal("audit child must be killed before completing the handoff")
}
