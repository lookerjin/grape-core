package trpcruntime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/taskrun/inprocess"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// Compare entry points without executing any tool.
func TestTaskRunEntryPoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observations := make(chan string, 2)
	mdl := mockmodel.New("probe", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		inv, ok := trpcagent.InvocationFromContext(ctx)
		observations <- fmt.Sprintf("policy=%t state=%t", ok && inv.RunOptions.ToolPermissionPolicy != nil, stateFrom(ctx) != nil)
		return mockmodel.Echo(ctx, req)
	})
	rt := newTestRuntime(t, mdl, quietConfig(t))
	defer rt.Close()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, h)
	svc, err := inprocess.NewService(rt.runner)
	if err != nil {
		t.Fatal(err)
	}
	svc.Start(ctx)
	defer svc.Close()
	run, err := svc.Spawn(ctx, inprocess.SpawnRequest{OwnerUserID: "u", ParentSessionID: "s", Task: "observe"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Wait(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations %d, native result %+v", len(observations), result)
	}
	a, b := <-observations, <-observations
	t.Logf("Start: %s; TaskRun: %s, child session=%s", a, b, result.ChildSessionID)
	if a != "policy=true state=true" || b != "policy=false state=false" {
		t.Fatalf("unexpected boundary: %s / %s", a, b)
	}
}
