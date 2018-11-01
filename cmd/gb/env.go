package main

import "github.com/SirBigG/gb/cmd"

func init() {
	registerCommand(envCmd)
}

var envCmd = &cmd.Command{
	Name:      "env",
	UsageLine: `env [var ...]`,
	Short:     "print project environment variables",
	Long: `
Env prints project environment variables. If one or more variable names is 
given as arguments, env prints the value of each named variable on its own line.
`,
	Run:           info,
	SkipParseArgs: true,
}
