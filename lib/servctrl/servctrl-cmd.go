package servctrl

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"msh/lib/errco"
	"msh/lib/model"
	"msh/lib/opsys"
	"msh/lib/servstats"
	"msh/lib/utility"
)

// ServTerm is the variable that represent the running minecraft server
var ServTerm *servTerminal = &servTerminal{IsActive: false}

// servTerminal is the minecraft server terminal
type servTerminal struct {
	IsActive  bool
	Wg        sync.WaitGroup // used to wait terminal StdoutPipe/StderrPipe
	startTime time.Time      // uptime of minecraft server
	cmd       *exec.Cmd
	outPipe   io.ReadCloser
	errPipe   io.ReadCloser
	inPipe    io.WriteCloser
}

// lastOut is a channel used to communicate the last line got from the printer function
var lastOut = make(chan string)

// Execute executes a command on ms.
// Commands with no terminal output don't cause hanging:
// a timeout is set to receive a new terminal output line after which Execute returns.
// [non-blocking]
func Execute(command, origin string) (string, *errco.MshLog) {
	// check if ms is running
	logMsh := checkMSRunning()
	if logMsh != nil {
		return "", logMsh.AddTrace()
	}

	errco.NewLogln(errco.TYPE_INF, errco.LVL_2, errco.ERROR_NIL, "ms command: %s%s%s\t(origin: %s)", errco.COLOR_YELLOW, command, errco.COLOR_RESET, origin)

	// write to server terminal (\n indicates the enter key)
	_, err := ServTerm.inPipe.Write([]byte(command + "\n"))
	if err != nil {
		return "", errco.NewLog(errco.TYPE_ERR, errco.LVL_2, errco.ERROR_PIPE_INPUT_WRITE, err.Error())
	}

	// read all lines from lastOut
	// (watchdog used in case there are no more lines to read or output takes too long)
	var out string = ""
a:
	for {
		select {
		case lo := <-lastOut:
			out += lo + "\n"
		case <-time.NewTimer(200 * time.Millisecond).C:
			break a
		}
	}

	// return the (possibly) full terminal output of Execute()
	return out, nil
}

// TellRaw executes a tellraw on ms
// [non-blocking]
func TellRaw(reason, text, origin string) *errco.MshLog {
	// check if ms is running
	logMsh := checkMSRunning()
	if logMsh != nil {
		return logMsh.AddTrace()
	}

	gameMessage, err := json.Marshal(&model.GameRawMessage{Text: "[MSH] " + reason + ": " + text, Color: "aqua", Bold: false})
	if err != nil {
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_2, errco.ERROR_JSON_MARSHAL, err.Error())
	}

	gameMessage = append([]byte("tellraw @a "), gameMessage...)
	gameMessage = append(gameMessage, []byte("\n")...)

	errco.NewLogln(errco.TYPE_INF, errco.LVL_2, errco.ERROR_NIL, "ms tellraw: %s%s%s\t(origin: %s)", errco.COLOR_YELLOW, string(gameMessage), errco.COLOR_RESET, origin)

	// write to server terminal (\n indicates the enter key)
	_, err = ServTerm.inPipe.Write(gameMessage)
	if err != nil {
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_2, errco.ERROR_PIPE_INPUT_WRITE, err.Error())
	}

	return nil
}

// TermUpTime returns the current minecraft server uptime.
// In case of error 0 is returned.
func TermUpTime() int {
	if !ServTerm.IsActive {
		return 0
	}

	return utility.RoundSec(time.Since(ServTerm.startTime))
}

// checkMSRunning checks if minecraft server is running and it's possible to interact with it.
//
// checks if terminal is active, ms status is online and ms process not suspended.
func checkMSRunning() *errco.MshLog {
	switch {
	case !ServTerm.IsActive:
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_2, errco.ERROR_TERMINAL_NOT_ACTIVE, "terminal not active")
	case servstats.Stats.Status != errco.SERVER_STATUS_ONLINE:
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_2, errco.ERROR_SERVER_NOT_ONLINE, "server not online")
	case servstats.Stats.Suspended:
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_2, errco.ERROR_SERVER_SUSPENDED, "server is suspended")
	}

	return nil
}

// termStart starts a new terminal.
// If server terminal is already active it returns without doing anything
// [non-blocking]
func termStart(dir, command string) *errco.MshLog {
	if ServTerm.IsActive {
		errco.NewLogln(errco.TYPE_WAR, errco.LVL_3, errco.ERROR_SERVER_IS_WARM, "minecraft server terminal already active")
		return nil
	}

	logMsh := termLoad(dir, command)
	if logMsh != nil {
		return logMsh.AddTrace()
	}

	go printerOutErr()

	err := ServTerm.cmd.Start()
	if err != nil {
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_TERMINAL_START, err.Error())
	}

	go waitForExit()

	// initialization
	servstats.Stats.LoadProgress = "0%"
	servstats.Stats.PlayerCount = 0

	return nil
}

// termLoad loads cmd/pipes into ServTerm
func termLoad(dir, command string) *errco.MshLog {
	cSplit := strings.Split(command, " ")

	// set terminal cmd
	ServTerm.cmd = exec.Command(cSplit[0], cSplit[1:]...)
	ServTerm.cmd.Dir = dir

	// launch as new process group so that signals (ex: SIGINT) are sent to msh
	// (not relayed to the java server child process)
	ServTerm.cmd.SysProcAttr = opsys.NewProcGroupAttr()

	// set terminal pipes
	var err error
	ServTerm.outPipe, err = ServTerm.cmd.StdoutPipe()
	if err != nil {
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_PIPE_LOAD, "StdoutPipe load: "+err.Error())
	}
	ServTerm.errPipe, err = ServTerm.cmd.StderrPipe()
	if err != nil {
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_PIPE_LOAD, "StderrPipe load: "+err.Error())
	}
	ServTerm.inPipe, err = ServTerm.cmd.StdinPipe()
	if err != nil {
		return errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_PIPE_LOAD, "StdinPipe load: "+err.Error())
	}

	return nil
}

// printerOutErr manages the communication from StdoutPipe/StderrPipe.
// Launches 1 goroutine to scan StdoutPipe and 1 goroutine to scan StderrPipe
// (Should be called before cmd.Start())
// [goroutine]
func printerOutErr() {
	// add printer-out + printer-err to waitgroup
	ServTerm.Wg.Add(2)

	// print terminal StdoutPipe
	// [goroutine]
	go func() {
		var line string

		defer ServTerm.Wg.Done()

		scanner := bufio.NewScanner(ServTerm.outPipe)

		for scanner.Scan() {
			line = scanner.Text()

			errco.NewLogln(errco.TYPE_SER, errco.LVL_2, errco.ERROR_NIL, line)

			// communicate to lastOut so that func Execute() can return the output of the command.
			// must be a non-blocking select or it might cause hanging
			select {
			case lastOut <- line:
			default:
			}

			switch servstats.Stats.Status {

			case errco.SERVER_STATUS_STARTING:
				// for modded server terminal compatibility, use separate check for "INFO" and flag-word
				// using only "INFO" and not "[Server thread/INFO]"" because paper minecraft servers don't use "[Server thread/INFO]"

				// "Preparing spawn area: " -> update ServStats.LoadProgress
				if strings.Contains(line, "INFO") && strings.Contains(line, "Preparing spawn area: ") {
					servstats.Stats.LoadProgress = strings.Split(strings.Split(line, "Preparing spawn area: ")[1], "\n")[0]
				}

				// ": Done (" -> set ServStats.Status = ONLINE
				// using ": Done (" instead of "Done" to avoid false positives (issue #112)
				if strings.Contains(line, "INFO") && strings.Contains(line, ": Done (") {
					servstats.Stats.Status = errco.SERVER_STATUS_ONLINE
					errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "MINECRAFT SERVER IS ONLINE!")

					// schedule soft freeze of ms
					// (if no players connect the server will shutdown)
					FreezeMSSchedule()
				}

			case errco.SERVER_STATUS_ONLINE:
				// It is possible that a player could send a message that contains text similar to server output:
				// 		[14:08:43] [Server thread/INFO]: <player> Stopping
				// 		[14:09:32] [Server thread/INFO]: [player] Stopping
				//
				// These are the correct shutdown logs:
				// 		[14:09:46] [Server thread/INFO]: Stopping the server
				// 		[15Mar2021 14:09:46.581] [Server thread/INFO] [net.minecraft.server.dedicated.DedicatedServer/]: Stopping the server
				//
				// lineSplit is therefore implemented:
				//
				// [14:09:46] [Server thread/INFO]: <player> ciao
				// ^-----------header------------^##^--content--^

				// Continue if line does not contain ": "
				// (it does not adhere to expected log format or it is a multiline java exception)
				if !strings.Contains(line, ": ") {
					errco.NewLogln(errco.TYPE_WAR, errco.LVL_2, errco.ERROR_SERVER_UNEXP_OUTPUT, "line does not adhere to expected log format")
					continue
				}

				lineSplit := strings.SplitN(line, ": ", 2)
				lineHeader := lineSplit[0]
				lineContent := lineSplit[1]

				if strings.Contains(lineHeader, "INFO") {
					switch {
					// player sends a chat message
					case strings.HasPrefix(lineContent, "<") || strings.HasPrefix(lineContent, "["):
						// just log that the line is a chat message
						errco.NewLogln(errco.TYPE_INF, errco.LVL_2, errco.ERROR_NIL, "a chat message was sent")

					// player joins the server
					// using "UUID of player" since minecraft server v1.12.2 does not use "joined the game"
					case strings.Contains(lineContent, "UUID of player"):
						servstats.Stats.PlayerCount++
						errco.NewLogln(errco.TYPE_INF, errco.LVL_2, errco.ERROR_NIL, "A PLAYER JOINED THE SERVER! - %d players online", servstats.Stats.PlayerCount)

					// player leaves the server
					// using "lost connection" (instead of "left the game") because it's more general (issue #116)
					case strings.Contains(lineContent, "lost connection"):
						servstats.Stats.PlayerCount--
						errco.NewLogln(errco.TYPE_INF, errco.LVL_2, errco.ERROR_NIL, "A PLAYER LEFT THE SERVER! - %d players online", servstats.Stats.PlayerCount)
						// schedule soft freeze of ms
						FreezeMSSchedule()

					// the server is stopping
					case strings.Contains(lineContent, "Stopping") && strings.Contains(lineContent, "server"):
						servstats.Stats.Status = errco.SERVER_STATUS_STOPPING
						errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "MINECRAFT SERVER IS STOPPING!")
					}
				}
			}
		}
	}()

	// print terminal StderrPipe
	// [goroutine]
	go func() {
		var line string

		defer ServTerm.Wg.Done()

		scanner := bufio.NewScanner(ServTerm.errPipe)

		for scanner.Scan() {
			line = scanner.Text()

			errco.NewLogln(errco.TYPE_SER, errco.LVL_2, errco.ERROR_NIL, line)
		}
	}()
}

// waitForExit manages ServTerm.isActive parameter and set ServStats.Status = OFFLINE when minecraft server process exits.
// [goroutine]
func waitForExit() {
	servstats.Stats.Status = errco.SERVER_STATUS_STARTING
	errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "MINECRAFT SERVER IS STARTING!")

	ServTerm.IsActive = true
	errco.NewLogln(errco.TYPE_INF, errco.LVL_3, errco.ERROR_NIL, "waitForExit: terminal started")

	// set terminal start time
	ServTerm.startTime = time.Now()

	// wait for server process to finish
	ServTerm.Wg.Wait()  // wait terminal StdoutPipe/StderrPipe to exit
	ServTerm.cmd.Wait() // wait process (to avoid defunct java server process)

	ServTerm.outPipe.Close()
	ServTerm.errPipe.Close()
	ServTerm.inPipe.Close()

	ServTerm.IsActive = false
	errco.NewLogln(errco.TYPE_INF, errco.LVL_3, errco.ERROR_NIL, "waitForExit: terminal exited")

	servstats.Stats.Status = errco.SERVER_STATUS_OFFLINE
	servstats.Stats.Suspended = false

	errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "MINECRAFT SERVER IS OFFLINE!")
}
