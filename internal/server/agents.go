package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// touch records that a's session is present. Presence is best effort: a
// failure is logged and never fails the connection or the request that
// caused it.
func (pr *served) touch(ctx context.Context, log *slog.Logger, a store.Actor, harness string) {
	if !store.ValidHarness(harness) {
		log.InfoContext(ctx, "ignored harness", "event", logs.AgentHarness, "harness", fmt.Sprintf("%.40q", harness))
		harness = ""
	}
	tctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	if err := pr.store.TouchAgent(tctx, a, harness); err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "register agent", "event", logs.AgentRegister, "err", err)
	}
}

func who(ctx context.Context, pr *served, _ store.Actor, in proto.WhoArgs) (any, *proto.Error) {
	var since time.Duration
	if in.Since != "" {
		d, err := proto.ParseDuration(in.Since)
		if err != nil || d > store.MaxAgentWindow {
			return nil, proto.Errf(proto.CodeInvalid, "give a window up to 7d, such as 15m, 2h or 1d",
				fmt.Sprintf("since %q is not a duration up to 7d", in.Since))
		}
		since = d
	}
	now := pr.store.Now()
	agents, more, err := pr.store.WhoPage(ctx, since, in.Limit)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpWho, "", 0, err)
	}
	out := proto.WhoResult{Now: now, Agents: []proto.Agent{}, More: more}
	for _, a := range agents {
		w := proto.Agent{Principal: a.Principal, Session: a.Session, Machine: a.Machine, Harness: a.Harness,
			Started: a.Started, LastSeen: a.LastSeen}
		for _, id := range a.Claims {
			w.Claims = append(w.Claims, string(id))
		}
		out.Agents = append(out.Agents, w)
	}
	return out, nil
}
