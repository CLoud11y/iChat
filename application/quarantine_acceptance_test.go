package application

import (
	"context"
	"testing"
	"time"

	"iChat/config"
	"iChat/messaging/rabbitmq"
)

func TestQuarantineAndPersistenceSourcesRemainIndependent(t *testing.T) {
	r, _ := sourceTestResources(t)
	inputs, err := runtimeRoleInputs(r, mustSourcePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.persistence.claim(inputs[RolePersistence].Consumers)
	if err != nil {
		t.Fatal(err)
	}
	q, err := r.quarantine.claim(inputs[RoleQuarantine].Consumers)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pg, err := p(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qg, err := q(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check := func(pcount, qcount int, ready bool) {
		t.Helper()
		r.mu.Lock()
		consumers, gotReady := r.consumerSnapshotLocked()
		r.mu.Unlock()
		counts := map[string]int{}
		for _, c := range consumers {
			counts[c.Queue]++
		}
		if counts[rabbitmq.PersistQueue] != pcount || counts[rabbitmq.QuarantineQueue] != qcount || gotReady != ready {
			t.Fatal("role generation masked another role", counts, gotReady)
		}
	}
	check(2, 1, true)
	if err := pg.Close(ctx); err != nil {
		t.Fatal(err)
	}
	check(0, 1, false)
	r.persistence.dial = func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
		return newJoinedTestConsumer(), nil
	}
	pg, err = p(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check(2, 1, true)
	if err := qg.Close(ctx); err != nil {
		t.Fatal(err)
	}
	check(2, 0, false)
	r.quarantine.dial = func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
		return newJoinedTestConsumer(), nil
	}
	qg, err = q(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check(2, 1, true)
	if err := pg.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := qg.Close(ctx); err != nil {
		t.Fatal(err)
	}
	check(0, 0, false)
}
