package director

import (
	"github.com/mwitkow/go-grpc-middleware/logging"
	"github.com/mwitkow/grpc-proxy/proxy"
	"github.com/mwitkow/kedge/grpc/backendpool"
	"github.com/mwitkow/kedge/grpc/director/router"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// New builds a StreamDirector based off a backend pool and a router.
func New(pool backendpool.Pool, router router.Router) proxy.StreamDirector {
	return func(ctx context.Context, fullMethodName string) (*grpc.ClientConn, error) {
		beName, err := router.Route(ctx, fullMethodName)
		if err != nil {
			return nil, err
		}
		grpc_logging.ExtractMetadata(ctx).AddFieldsFromMiddleware([]string{"proxy_backend"}, []interface{}{beName})
		cc, err := pool.Conn(beName)
		if err != nil {
			return nil, err
		}
		return cc, nil
	}
}
