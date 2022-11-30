package commands

import (
	"github.com/NHAS/reverse_ssh/pkg/logger"
	"github.com/NHAS/reverse_ssh/sources"
	"github.com/QU35T-code/reverse_ssh/sources/server/multiplexer"
	"github.com/QU35T-code/reverse_ssh/sources/terminal"
)

// This is used for help, so we can generate the nice table
// I would prefer if we could do some sort of autoregistration process for these
var allCommands = map[string]terminal.Command{
	"ls":      &list{},
	"help":    &help{},
	"kill":    &kill{},
	"connect": &connect{},
	"exit":    &exit{},
	"link":    &link{},
	"exec":    &exec{},
	"who":     &who{},
	"watch":   &watch{},
	"listen":  &listen{},
	"webhook": &webhook{},
	"version": &version{},
}

func CreateCommands(user *sources.User, log logger.Logger) map[string]terminal.Command {

	var o = map[string]terminal.Command{
		"ls":      &list{},
		"help":    &help{},
		"kill":    Kill(log),
		"connect": Connect(user, log),
		"exit":    &exit{},
		"link":    &link{},
		"exec":    &exec{},
		"who":     &who{},
		"watch":   &watch{},
		"listen":  Listen(multiplexer.ServerMultiplexer, log),
		"webhook": &webhook{},
		"version": &version{},
	}

	return o
}
