package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os/exec"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// handleTerminal bridges a browser terminal to tmux in the agent's container. tmux
// keeps the session alive across dropped connections, so a sign-in survives a phone
// losing signal. The stream can hold secrets and is never logged. Credentials aren't
// in this environment either, which also lets a subscription login take effect.
func (a *App) handleTerminal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.agent(id); err != nil {
		apiError(w, http.StatusNotFound, "No such agent")
		return
	}
	// A busy agent's container already exists. Checking it here could recreate it for
	// a new image, which would kill the run.
	if !a.isRunning(id) {
		if err := a.ensureContainer(r.Context(), id); err != nil {
			apiError(w, http.StatusInternalServerError, "Couldn't start the container: "+err.Error())
			return
		}
	}
	// Rejects other origins by default.
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx := r.Context()

	cmd := exec.Command("docker", "exec", "-it", "-e", "TERM=xterm-256color", containerName(id), "tmux", "-u", "new", "-A", "-D", "-s", "main")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		log.Printf("terminal %d: %v", id, err)
		c.Close(websocket.StatusInternalError, "couldn't start the terminal")
		return
	}
	defer func() {
		ptmx.Close()
		cmd.Process.Kill() // ends this client; tmux and its session live on
		cmd.Wait()
	}()

	go func() {
		defer c.CloseNow()
		buf := make([]byte, 32<<10)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText { // control: {"cols":..,"rows":..}
			var size struct {
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &size) == nil && size.Cols > 0 && size.Rows > 0 {
				pty.Setsize(ptmx, &pty.Winsize{Rows: size.Rows, Cols: size.Cols})
			}
			continue
		}
		if _, err := ptmx.Write(data); err != nil {
			return
		}
	}
}
