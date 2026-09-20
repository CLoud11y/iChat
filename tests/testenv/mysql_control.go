package testenv

import (
	"context"
	"errors"
	"net"
	"strings"
)

// SetMySQLRunning controls only this run's existing MySQL container. It never
// recreates the container. Docker can assign a new random host port on restart;
// Config is refreshed accordingly. Calls must serialize with other Environment
// operations; starting does not imply MySQL is ready.
func (e *Environment) SetMySQLRunning(ctx context.Context, running bool) error {
	if !e.owned || !e.ready || !validRunID.MatchString(e.runID) {
		return errors.New("refusing mysql control outside an initialized owned test run")
	}
	output, err := e.run(ctx, e.composeArgs("ps", "--all", "--quiet", "mysql")...)
	if err != nil {
		return err
	}
	ids := strings.Fields(string(output))
	if len(ids) != 1 {
		return errors.New("expected exactly one test mysql container")
	}
	if err := e.verify(ctx, []resource{{kind: "container", id: ids[0]}}); err != nil {
		return err
	}
	args := []string{"stop", "--time", "10", ids[0]}
	if running {
		args = []string{"start", ids[0]}
	}
	_, err = e.run(ctx, args...)
	if err == nil && running {
		address, addressErr := e.address(ctx, "mysql", "3306")
		if addressErr != nil {
			return addressErr
		}
		e.conf.MYSQL.Host, e.conf.MYSQL.Port, _ = net.SplitHostPort(address)
	}
	return err
}
