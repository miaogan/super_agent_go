package handler

import (
	"context"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	"github.com/aegis/pkg/identity"
)

// contextWithIdentity 构造携带身份与租户的 context
func contextWithIdentity(uid int64, tenant string) context.Context {
	return identity.With(context.Background(), &commonv1.Identity{
		Uid:      uid,
		Username: "u",
		Tenant:   tenant,
		Scopes:   []string{"user"},
	})
}
