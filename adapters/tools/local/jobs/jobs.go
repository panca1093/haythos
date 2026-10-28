package jobs

import (
	"context"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/app"
)

type Kernel interface {
	DefineJob(ctx context.Context, def app.JobDef) error
	SetJobEnabled(ctx context.Context, name string, enabled bool) error
	DeleteJob(ctx context.Context, name string) error
	ListJobs(ctx context.Context) ([]app.JobStatus, error)
}

func Register(r *local.Registry, k Kernel) {}
