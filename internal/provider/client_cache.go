package provider

import (
	"context"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

// clientCache lazily connects to, and reuses, one *sqlclient.Client per
// distinct sqlclient.Target. Resources resolve their own target (falling
// back to the provider's default server/database when unset), so several
// mssql_user instances pointed at the same server/database share a
// single connection pool instead of each opening their own.
//
// A single clientCache instance is shared by every resource Configure
// call for one provider instance, so its methods must be safe for
// concurrent use — Terraform evaluates resources in parallel.
type clientCache struct {
	credential azcore.TokenCredential

	mu      sync.Mutex
	clients map[string]*sqlclient.Client
}

func newClientCache(credential azcore.TokenCredential) *clientCache {
	return &clientCache{
		credential: credential,
		clients:    make(map[string]*sqlclient.Client),
	}
}

// Get returns the cached client for target, connecting lazily on first
// use. Connecting happens outside the cache's lock so that establishing a
// connection to one target never blocks lookups/inserts for others; on
// the rare race where two callers connect to the same new target
// concurrently, the loser's connection is closed and the winner's is
// reused.
func (c *clientCache) Get(ctx context.Context, target sqlclient.Target) (*sqlclient.Client, error) {
	key := target.Key()

	c.mu.Lock()
	if client, ok := c.clients[key]; ok {
		c.mu.Unlock()
		return client, nil
	}
	c.mu.Unlock()

	client, err := sqlclient.Connect(ctx, c.credential, target)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.clients[key]; ok {
		client.Close()
		return existing, nil
	}
	c.clients[key] = client
	return client, nil
}
