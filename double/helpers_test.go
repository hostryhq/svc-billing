package double_test

import (
	"context"

	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-billing/internal/seed"
)

func tenantCtx(ctx context.Context) context.Context {
	return tenancy.With(ctx, tenancy.Context{TenantID: ids.ID(seed.Tenant)})
}
