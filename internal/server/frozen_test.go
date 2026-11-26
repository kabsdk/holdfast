package server

import (
	"context"
	"testing"

	"github.com/kabsdk/holdfast/internal/gateway"
)

// While its repository is rolled back, a host may read it but not write: the gateway freezes it (decision 058).
func TestGatewayFrozenDuringRollback(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	if a := s.gatewayAccess(ctx, "web01.example.com", "web01.example.com"); a != gateway.Own {
		t.Fatalf("its own repository: %d", a)
	}
	s.frozen.Store("web01.example.com", true)
	if a := s.gatewayAccess(ctx, "web01.example.com", "web01.example.com"); a != gateway.Frozen {
		t.Fatalf("its own repository while it is rolled back: %d", a)
	}
	s.frozen.Delete("web01.example.com")
	if a := s.gatewayAccess(ctx, "web01.example.com", "web01.example.com"); a != gateway.Own {
		t.Fatalf("its own repository once the rollback is done: %d", a)
	}
}
