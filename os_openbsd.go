//go:build openbsd
// +build openbsd

package wgctrl

import (
	"github.com/danpashin/wgctrl/internal/wginternal"
	"github.com/danpashin/wgctrl/internal/wgopenbsd"
	"github.com/danpashin/wgctrl/internal/wguser"
	"github.com/danpashin/wgctrl/wgtypes"
)

// newClients configures wginternal.Clients for OpenBSD systems.
func newClients(clientType wgtypes.ClientType) ([]wginternal.Client, error) {
	var clients []wginternal.Client

	// OpenBSD has an in-kernel WireGuard implementation. Determine if it is
	// available and make use of it if so.
	kc, ok, err := wgopenbsd.New()
	if err != nil {
		return nil, err
	}
	if ok {
		clients = append(clients, kc)
	}

	uc, err := wguser.New(clientType)
	if err != nil {
		return nil, err
	}

	clients = append(clients, uc)
	return clients, nil
}
