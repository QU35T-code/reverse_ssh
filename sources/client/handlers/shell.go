//go:build !windows
// +build !windows

package handlers

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/QU35T-code/reverse_ssh/pkg/logger"
	"github.com/QU35T-code/reverse_ssh/sources"
	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

var shells []string

func init() {

	var potentialShells []string
	file, err := os.Open("/etc/shells")
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()

			if len(line) > 0 && line[0] == '#' || strings.TrimSpace(line) == "" {
				continue
			}
			potentialShells = append(potentialShells, strings.TrimSpace(line))
		}
	} else {
		//If the host did not have a /etc/shells, guess a few common shells
		potentialShells = []string{
			"/bin/bash",
			"/bin/zsh",
			"/bin/ash",
			"/bin/sh",
		}

	}
	for _, s := range potentialShells {

		if stats, err := os.Stat(s); err != nil && (os.IsNotExist(err) || !stats.IsDir()) {
			continue

		}
		shells = append(shells, s)
	}

}

func runCommandWithPty(command string, args []string, user *sources.User, requests <-chan *ssh.Request, log logger.Logger, connection ssh.Channel) {

	if user.Pty == nil {
		log.Error("Requested to run a command with a pty, but did not start a pty")
		return
	}

	// Fire up a shell for this session
	shell := exec.Command(command, args...)
	shell.Env = os.Environ()

	close := func() {
		connection.Close()
		if shell.Process != nil {

			err := shell.Process.Kill()
			if err != nil {
				log.Warning("Failed to kill shell(%s)", err)
			}
		}

		log.Info("Session closed")
	}

	// Allocate a terminal for this channel
	var err error
	var shellIO io.ReadWriteCloser

	shell.Env = append(shell.Env, "TERM="+user.Pty.Term)

	log.Info("Creating pty...")
	shellIO, err = pty.StartWithSize(shell, &pty.Winsize{Cols: uint16(user.Pty.Columns), Rows: uint16(user.Pty.Rows)})
	if err != nil {
		log.Info("Could not start pty (%s)", err)
		close()
		return
	}

	// pipe session to bash and visa-versa
	var once sync.Once
	go func() {
		io.Copy(connection, shellIO)
		once.Do(close)
	}()
	go func() {
		io.Copy(shellIO, connection)
		once.Do(close)
	}()

	go func() {
		for req := range requests {
			switch req.Type {

			case "window-change":
				if shellf, ok := shellIO.(*os.File); ok {
					w, h := sources.ParseDims(req.Payload)
					err = pty.Setsize(shellf, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
					if err != nil {
						log.Warning("Unable to set terminal size: %s", err)
					}
				}

			default:
				log.Warning("Unknown request %s", req.Type)
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
	}()

	defer once.Do(close)
	shell.Wait()
}

// This basically handles exactly like a SSH server would
func shell(user *sources.User, connection ssh.Channel, requests <-chan *ssh.Request, log logger.Logger) {

	path := ""
	if len(shells) != 0 {
		path = shells[0]
	}

	if user.Pty != nil {
		runCommandWithPty(path, nil, user, requests, log, connection)
		return
	}

	runCommand(path, nil, connection)

}
