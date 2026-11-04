// Package inventory answers the inventory.v1.InventoryService calls from the warehouse's
// own database.
package inventory

import inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"

// Server implements inventory.v1.InventoryService. GetStock, Reserve and Release come with
// task R2-04; until then the embedded type answers each of them with UNIMPLEMENTED.
type Server struct {
	inventoryv1.UnimplementedInventoryServiceServer
}

// NewServer returns the service that main registers on the gRPC server.
func NewServer() *Server {
	return &Server{}
}
