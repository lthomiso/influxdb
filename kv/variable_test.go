package kv_test

import (
	"context"
	"testing"

	"github.com/influxdata/influxdb"
	"github.com/influxdata/influxdb/kv"
	influxdbtesting "github.com/influxdata/influxdb/testing"
	"go.uber.org/zap/zaptest"
)

func TestBoltVariableService(t *testing.T) {
	influxdbtesting.VariableService(initBoltVariableService, t)
}

func initBoltVariableService(f influxdbtesting.VariableFields, t *testing.T) (influxdb.VariableService, string, func()) {
	s, closeBolt, err := NewTestBoltStore(t)
	if err != nil {
		t.Fatalf("failed to create new kv store: %v", err)
	}

	svc, op, closeSvc := initVariableService(s, f, t)
	return svc, op, func() {
		closeSvc()
		closeBolt()
	}
}

func initVariableService(s kv.Store, f influxdbtesting.VariableFields, t *testing.T) (influxdb.VariableService, string, func()) {
	svc := kv.NewService(zaptest.NewLogger(t), s)
	svc.IDGenerator = f.IDGenerator

	svc.TimeGenerator = f.TimeGenerator

	if svc.TimeGenerator == nil {
		svc.TimeGenerator = influxdb.RealTimeGenerator{}
	}
	ctx := context.Background()
	if err := svc.Initialize(ctx); err != nil {
		t.Fatalf("error initializing variable service: %v", err)
	}
	for _, variable := range f.Variables {
		if err := svc.ReplaceVariable(ctx, variable); err != nil {
			t.Fatalf("failed to populate test variables: %v", err)
		}
	}

	done := func() {
		for _, variable := range f.Variables {
			if err := svc.DeleteVariable(ctx, variable.ID); err != nil {
				t.Logf("failed to clean up variables bolt test: %v", err)
			}
		}
	}

	return svc, kv.OpPrefix, done
}
