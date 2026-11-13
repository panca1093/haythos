package main

import (
	"fmt"

	"github.com/panca/haythos/adapters/config"
	"github.com/panca/haythos/adapters/runtime/acp"
	"github.com/panca/haythos/adapters/runtime/claudecli"
	"github.com/panca/haythos/adapters/runtime/codexapp"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
)

// buildRuntimes turns the runtimes block into ports and one guard per alias. A new kind is a case here plus its name in config.RuntimeKinds.
func buildRuntimes(cfg config.Main) (map[string]app.Runtime, map[string]policy.Guard, error) {
	rts := map[string]app.Runtime{}
	guards := map[string]policy.Guard{}
	for alias, rt := range cfg.Runtimes {
		switch rt.Kind {
		case "acp":
			rts[alias] = acp.New(acp.Config{Name: alias, Command: rt.Command, Args: rt.Args, PassEnv: rt.PassEnv, Env: rt.Env, ConfigOptions: rt.ConfigOptions})
		case "claudecli":
			rts[alias] = claudecli.New(claudecli.Config{Command: rt.Command, Model: rt.Model, ExtraArgs: rt.Args, PassEnv: rt.PassEnv})
		case "codexapp":
			rts[alias] = codexapp.New(codexapp.Config{Name: alias, Command: rt.Command, Args: rt.Args, Model: rt.Model, PassEnv: rt.PassEnv, Env: rt.Env})
		default:
			return nil, nil, fmt.Errorf("runtime %s: unknown kind %q", alias, rt.Kind)
		}
		g, err := policy.NewGuard(rt.GuardRules())
		if err != nil {
			return nil, nil, fmt.Errorf("runtime %s: %w", alias, err)
		}
		guards[alias] = g
	}
	return rts, guards, nil
}
