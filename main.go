package main

import (
	"context"
	"fmt"
	"io"
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
)

func main() {
	host := getEnv("HOST", "0.0.0.0")
	sshPort := getEnv("PORT", "2222")
	httpPort := getEnv("HTTP_PORT", "8080")

	// SSH server
	opts := []ssh.Option{
		wish.WithAddress(fmt.Sprintf("%s:%s", host, sshPort)),
		wish.WithMiddleware(
			quienMiddleware(),
			activeterm.Middleware(),
		),
	}
	if hostKey := os.Getenv("HOST_KEY"); hostKey != "" {
		opts = append(opts, wish.WithHostKeyPEM([]byte(hostKey)))
	} else {
		opts = append(opts, wish.WithHostKeyPath(getEnv("HOST_KEY_PATH", ".ssh/host_key")))
	}
	sshSrv, err := wish.NewServer(opts...)
	if err != nil {
		log.Fatalf("could not create SSH server: %s", err)
	}

	// HTTP server — redirect to GitHub
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://github.com/retlehs/quien", http.StatusFound)
	})
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
