package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

//go:embed static/*
var static embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	host := getEnv("HOST", "0.0.0.0")
	sshPort := getEnv("PORT", "2222")
	httpPort := getEnv("HTTP_PORT", "8080")
	hostKeyPath := getEnv("HOST_KEY_PATH", ".ssh/host_key")

	// SSH server
	sshSrv, err := wish.NewServer(
		wish.WithAddress(fmt.Sprintf("%s:%s", host, sshPort)),
		wish.WithHostKeyPath(hostKeyPath),
		wish.WithMiddleware(
			quienMiddleware(),
			activeterm.Middleware(),
		),
	)
	if err != nil {
		log.Fatalf("could not create SSH server: %s", err)
	}

	// HTTP server
	staticFS, _ := fs.Sub(static, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleWebSocket)
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("%s:%s", host, httpPort),
		Handler: mux,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	log.Printf("starting SSH server on %s:%s", host, sshPort)
	log.Printf("starting HTTP server on %s:%s", host, httpPort)

	go func() {
		if err := sshSrv.ListenAndServe(); err != nil {
			log.Fatalf("SSH server error: %s", err)
		}
	}()
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %s", err)
		}
	}()

	<-done
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sshSrv.Shutdown(ctx)
	httpSrv.Shutdown(ctx)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	cmd := exec.Command("quien")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		log.Printf("pty start error: %v", err)
		return
	}
	defer ptmx.Close()

	// PTY -> WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				// PTY closed (quien exited) — close WebSocket to trigger reconnect
				conn.Close()
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WebSocket -> PTY
	for {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		switch msgType {
		case websocket.BinaryMessage, websocket.TextMessage:
			if len(msg) > 0 && msg[0] == 1 && len(msg) >= 5 {
				cols := uint16(msg[1])<<8 | uint16(msg[2])
				rows := uint16(msg[3])<<8 | uint16(msg[4])
				pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			} else {
				ptmx.Write(msg)
			}
		}
	}

	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
}

func quienMiddleware() wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			ptyReq, winCh, _ := s.Pty()

			args := s.Command()
			cmdArgs := append([]string{}, args...)

			// When args are provided, use non-interactive mode to avoid
			// a race in quien's TUI where View is called before
			// WindowSizeMsg arrives (causes panic on zero width).
			// The prompt mode (no args) works fine interactively.
			if len(cmdArgs) > 0 {
				cmd := exec.CommandContext(s.Context(), "quien", cmdArgs...)
				cmd.Env = append(os.Environ(),
					fmt.Sprintf("TERM=%s", ptyReq.Term),
					fmt.Sprintf("COLUMNS=%d", ptyReq.Window.Width),
				)
				cmd.Stdout = s
				cmd.Stderr = s.Stderr()
				if err := cmd.Run(); err != nil {
					log.Printf("command error: %v", err)
				}
				next(s)
				return
			}

			cmd := exec.CommandContext(s.Context(), "quien", cmdArgs...)
			cmd.Env = append(os.Environ(), fmt.Sprintf("TERM=%s", ptyReq.Term))

			rows := uint16(ptyReq.Window.Height)
			cols := uint16(ptyReq.Window.Width)
			if rows == 0 {
				rows = 24
			}
			if cols == 0 {
				cols = 80
			}

			ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
				Rows: rows,
				Cols: cols,
			})
			if err != nil {
				log.Printf("pty start error: %v", err)
				wish.Fatalln(s, fmt.Sprintf("failed to start: %v", err))
				return
			}
			defer ptmx.Close()

			go func() {
				for win := range winCh {
					pty.Setsize(ptmx, &pty.Winsize{
						Rows: uint16(win.Height),
						Cols: uint16(win.Width),
					})
				}
			}()

			go io.Copy(ptmx, s)
			io.Copy(s, ptmx)

			cmd.Wait()
			next(s)
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
