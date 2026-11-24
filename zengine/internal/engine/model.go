// Package engine runs the agent: it owns a session, drives the model/tool
// loop, keeps requests valid and within limits, retries and pauses on network
// trouble, compacts context, and publishes live events to whichever
// interfaces are attached.
package engine

import (
	"context"

	"github.com/ranjanlive/zpilot/zengine/internal/provider"
)

// Model is the engine's view of a provider: encode a request, send it. It is
// an interface so tests can script model behaviour without a network.
type Model interface {
	Format() string
	Build(req provider.Request) (*provider.Built, error)
	Send(ctx context.Context, b *provider.Built, sink func(provider.Delta)) (*provider.Response, error)
}

type clientModel struct{ c *provider.Client }

// NewModel adapts a provider client to Model.
func NewModel(c *provider.Client) Model { return clientModel{c} }

func (m clientModel) Format() string { return m.c.Adapter.Format() }

func (m clientModel) Build(req provider.Request) (*provider.Built, error) {
	return m.c.Adapter.Build(req)
}

func (m clientModel) Send(ctx context.Context, b *provider.Built, sink func(provider.Delta)) (*provider.Response, error) {
	return m.c.Send(ctx, b, sink)
}
