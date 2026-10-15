package mcpserver

import (
	"context"

	"github.com/ariesworx/starfix/internal/proto"
)

// The gate tool (design §20.3; decision 76): an agent may hold work back
// for a person, as a planning agent gates its plan before filing the
// rest, and read which gates are open. Passing, waiving and rearming are
// a person's, at their own terminal, so the tool has no such action.

// gateActions are what the gate tool does.
var gateActions = []any{"add", "list"}

// GateIn adds a gate holding Blocks back for Approvers (none: the
// project's admins), or lists the open gates.
type GateIn struct {
	Action    string   `json:"action"`
	Blocks    string   `json:"blocks,omitempty"`
	Approvers []string `json:"approvers,omitempty"`
	Title     string   `json:"title,omitempty"`

	idem string
}

func (in *GateIn) prepare() error {
	if in.Action == "add" && in.Blocks == "" {
		return proto.Errf(proto.CodeInvalid, "", "add needs blocks, the issue the gate holds back")
	}
	in.idem = proto.NewIdem("mcp")
	return nil
}

// GateAdded is the gate an add made, and what follows from it.
type GateAdded struct {
	ID        string   `json:"id"`
	Approvers []string `json:"approvers"`
	Holds     []string `json:"holds,omitempty"`
	Next      string   `json:"next"`
}

// GateLine is one gate in a list, compact.
type GateLine struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Approvers []string `json:"approvers"`
	Holds     []string `json:"holds,omitempty"`
}

// Gates lists the open gates. More says some were left out to fit.
type Gates struct {
	untrusted
	Gates []GateLine `json:"gates"`
	More  bool       `json:"more,omitempty"`
}

func (r *Gates) mark() { r.set(len(r.Gates) > 0) }

// fit drops the last gates, leaving at least one, until r is under
// MaxResultTokens, and sets More if it dropped any.
func (r *Gates) fit() {
	dropLast(&r.Gates, &r.More, func() bool { return size(r) <= MaxResultTokens })
}

// gate is the gate tool's call: an add, or a list of the open gates.
func gate(ctx context.Context, c Conn, in GateIn) (any, error) {
	if in.Action == "list" {
		var r proto.GatesResult
		if err := c.Call(ctx, proto.OpGates, proto.GatesArgs{}, &r); err != nil {
			return nil, err
		}
		out := Gates{Gates: []GateLine{}}
		for _, g := range r.Gates {
			t, _ := cut(g.Title, primeTitleLen)
			out.Gates = append(out.Gates, GateLine{ID: g.ID, Title: t, Approvers: g.Approvers, Holds: g.Holds})
		}
		out.fit()
		out.mark()
		return out, nil
	}
	var g proto.Gate
	if err := c.Call(ctx, proto.OpGateAdd, proto.GateAddArgs{Blocks: in.Blocks, Title: in.Title, Approvers: in.Approvers,
		Idem: in.idem}, &g); err != nil {
		return nil, err
	}
	approvers := g.Approvers
	if approvers == nil {
		approvers = []string{}
	}
	return GateAdded{ID: g.ID, Approvers: approvers, Holds: g.Holds,
		Next: "a person passes it; until then " + in.Blocks + " is not ready"}, nil
}
