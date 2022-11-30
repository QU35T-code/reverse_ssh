package commands

import (
	"fmt"
	"io"

	"github.com/QU35T-code/reverse_ssh/sources"
	"github.com/QU35T-code/reverse_ssh/sources/terminal"
)

type version struct {
}

func (v *version) Run(tty io.ReadWriter, line terminal.ParsedLine) error {
	fmt.Fprintln(tty, sources.Version)
	return nil
}

func (v *version) Expect(line terminal.ParsedLine) []string {
	return nil
}

func (v *version) Help(explain bool) string {
	if explain {
		return "Give server build version"
	}

	return terminal.MakeHelpText("version")
}
