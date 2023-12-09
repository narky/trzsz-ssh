/*
MIT License

Copyright (c) 2023 Lonny Wong <lonnywong@qq.com>
Copyright (c) 2023 [Contributors](https://github.com/trzsz/trzsz-ssh/graphs/contributors)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package tssh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/trzsz/go-arg"
)

const kTsshVersion = "0.1.15"

func background(args *sshArgs, dest string) (bool, error) {
	if v := os.Getenv("TRZSZ-SSH-BACKGROUND"); v == "TRUE" {
		return false, nil
	}

	monitor := false
	if v := os.Getenv("TRZSZ-SSH-BG-MONITOR"); v == "TRUE" {
		monitor = true
	}
	env := os.Environ()
	if args.Reconnect && !monitor {
		env = append(env, "TRZSZ-SSH-BG-MONITOR=TRUE")
	} else {
		env = append(env, "TRZSZ-SSH-BACKGROUND=TRUE")
	}

	newArgs := os.Args
	if args.Destination == "" {
		newArgs = append(newArgs, dest)
	} else if args.Destination != dest {
		idx := -1
		count := 0
		for i, arg := range newArgs {
			if arg == args.Destination {
				idx = i
				count++
			}
		}
		if count != 1 {
			return true, fmt.Errorf("don't know how to replace the destination: %s => %s", args.Destination, dest)
		}
		newArgs[idx] = dest
	}

	sleepTime := time.Duration(0)
	for {
		cmd := exec.Cmd{
			Path:   os.Args[0],
			Args:   newArgs,
			Env:    env,
			Stderr: os.Stderr,
		}

		if err := cmd.Start(); err != nil {
			return true, fmt.Errorf("run in background failed: %v", err)
		}
		if !monitor {
			return true, nil
		}

		beginTime := time.Now()
		_ = cmd.Wait()
		if time.Since(beginTime) < 10*time.Second {
			if sleepTime < 10*time.Second {
				sleepTime += time.Second
			}
			time.Sleep(sleepTime)
		} else {
			sleepTime = 0
		}
	}
}

var onExitFuncs []func()

func cleanupOnExit() {
	for i := len(onExitFuncs) - 1; i >= 0; i-- {
		onExitFuncs[i]()
	}
}

var cleanupAfterLogined []func()

func cleanupForGC() {
	for i := len(cleanupAfterLogined) - 1; i >= 0; i-- {
		cleanupAfterLogined[i]()
	}
}

func parseRemoteCommand(args *sshArgs) (string, error) {
	command := args.Option.get("RemoteCommand")
	if args.Command != "" && command != "" && strings.ToLower(command) != "none" {
		return "", fmt.Errorf("cannot execute command-line and remote command")
	}
	if args.Command != "" {
		if len(args.Argument) == 0 {
			return args.Command, nil
		}
		return fmt.Sprintf("%s %s", args.Command, strings.Join(args.Argument, " ")), nil
	}
	if strings.ToLower(command) == "none" {
		return "", nil
	} else if command != "" {
		return command, nil
	}
	return getConfig(args.Destination, "RemoteCommand"), nil
}

var isTerminal bool = isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())

func parseCmdAndTTY(args *sshArgs) (cmd string, tty bool, err error) {
	cmd, err = parseRemoteCommand(args)
	if err != nil {
		return
	}

	if args.DisableTTY && args.ForceTTY {
		err = fmt.Errorf("cannot specify -t with -T")
		return
	}
	if args.DisableTTY {
		tty = false
		return
	}
	if args.ForceTTY {
		tty = true
		return
	}

	requestTTY := getConfig(args.Destination, "RequestTTY")
	switch strings.ToLower(requestTTY) {
	case "", "auto":
		tty = isTerminal && (cmd == "")
	case "no":
		tty = false
	case "force":
		tty = true
	case "yes":
		tty = isTerminal
	default:
		err = fmt.Errorf("unknown RequestTTY option: %s", requestTTY)
	}
	return
}

func TsshMain() int {
	var args sshArgs
	parser := arg.MustParse(&args)

	// debug log
	if args.Debug {
		enableDebugLogging = true
	}

	// cleanup on exit
	defer cleanupOnExit()

	// print message after stdin reset
	var err error
	defer func() {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\r\n", err)
		}
	}()

	// init user config
	if err = initUserConfig(args.ConfigFile); err != nil {
		return 1
	}

	// setup virtual terminal on Windows
	if isTerminal {
		if err = setupVirtualTerminal(); err != nil {
			return 2
		}
	}

	// execute local tools if necessary
	if code, quit := execLocalTools(&args); quit {
		return code
	}

	// choose ssh alias
	dest := ""
	quit := false
	if args.Destination == "" {
		if !isTerminal {
			parser.WriteHelp(os.Stderr)
			return 3
		}
		dest, quit, err = chooseAlias("")
	} else {
		dest, quit, err = predictDestination(args.Destination)
	}
	if quit {
		err = nil
		return 0
	}
	if err != nil {
		return 4
	}

	// run as background
	if args.Background {
		var parent bool
		parent, err = background(&args, dest)
		if err != nil {
			return 5
		}
		if parent {
			return 0
		}
	}
	args.Destination = dest
	args.originalDest = dest

	// start ssh program
	if err = sshStart(&args); err != nil {
		return 6
	}
	return 0
}

func sshStart(args *sshArgs) error {
	// parse cmd and tty
	command, tty, err := parseCmdAndTTY(args)
	if err != nil {
		return err
	}

	// ssh login
	client, session, serverIn, serverOut, err := sshLogin(args, tty)
	if err != nil {
		return err
	}
	defer client.Close()
	if session != nil {
		defer session.Close()
	}

	// stdio forward
	if args.StdioForward != "" {
		var wg *sync.WaitGroup
		wg, err = stdioForward(client, args.StdioForward)
		if err != nil {
			return err
		}
		cleanupForGC()
		wg.Wait()
		return nil
	}

	// no command
	if args.NoCommand {
		cleanupForGC()
		_ = client.Wait()
		return nil
	}

	// execute remote tools if necessary
	execRemoteTools(args, client)

	// run command or start shell
	if command != "" {
		if err := session.Start(command); err != nil {
			return fmt.Errorf("start command [%s] failed: %v", command, err)
		}
	} else {
		if err := session.Shell(); err != nil {
			return fmt.Errorf("start shell failed: %v", err)
		}
	}

	// make stdin raw
	if isTerminal && tty {
		state, err := makeStdinRaw()
		if err != nil {
			return err
		}
		defer resetStdin(state)
	}

	// enable trzsz
	if err := enableTrzsz(args, client, session, serverIn, serverOut, tty); err != nil {
		return err
	}

	// cleanup and wait for exit
	cleanupForGC()
	_ = session.Wait()
	if args.Background {
		_ = client.Wait()
	}
	return nil
}
