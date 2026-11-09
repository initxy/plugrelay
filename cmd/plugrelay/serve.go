package main

import (
	"github.com/initxy/plugrelay/internal/app"
	"github.com/initxy/plugrelay/internal/cli"
	"github.com/initxy/plugrelay/internal/demo"
)

func init() {
	cli.Serve = app.Run
	cli.Apply = app.ApplyDocument
	cli.Demo = demo.Run
}
