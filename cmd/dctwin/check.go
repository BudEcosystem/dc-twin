package main

import (
	"fmt"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/pack"
)

type registryT = pack.Registry

func check(args []string) error {
	stack, rest := flagVal(args, "--stack")
	if stack == "" || len(rest) != 1 {
		return fmt.Errorf("check --stack <name> <estate.yaml>")
	}
	reg := mustRegistry()
	est, _, err := compile.LoadEstate(rest[0])
	if err != nil {
		return err
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch(), AllowTCG: true})
	if err != nil {
		return err
	}
	verdicts, err := compile.Check(res.Model, reg, stack)
	if err != nil {
		return err
	}
	refused := false
	for _, v := range verdicts {
		fmt.Printf("%-10s %-14s %-40s %s\n", v.Verdict, v.Component, v.Capability, v.Detail)
		if v.Verdict == "missing" && v.Gate == "install" {
			refused = true
		}
	}
	if refused {
		return fmt.Errorf("an install-gated requirement is missing: the stack cannot be installed on this estate")
	}
	return nil
}
